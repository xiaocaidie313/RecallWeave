package ingest

import (
	"context"
	"encoding/json"
	"fmt"

	"recallweave/internal/types"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) syncTranscript(ctx context.Context, sourceTag types.SourceTag, name string, messages []types.Message, opt TranscriptOptions) (ImportResult, error) {
	var result ImportResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		source := types.TranscriptSource{SourceTag: sourceTag, ExternalSessionID: opt.ExternalSessionID}
		// The unique key also serializes competing first imports across processes.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&source).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("source_tag = ? AND external_session_id = ?", sourceTag, opt.ExternalSessionID).
			Take(&source).Error; err != nil {
			return err
		}
		var session types.Session
		if source.SessionID == nil {
			session = types.Session{SourceTag: sourceTag, Name: name, ExtractStatus: types.ExtractPending}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
			if err := tx.Model(&source).Update("session_id", session.ID).Error; err != nil {
				return err
			}
		} else {
			// Use the same Session lock as extraction when publishing new messages.
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND source_tag = ? AND conversation_id IS NULL", *source.SessionID, sourceTag).
				Take(&session).Error; err != nil {
				return err
			}
		}
		var saved []types.Message
		if err := tx.Where("session_id = ?", session.ID).Order("seq").Find(&saved).Error; err != nil {
			return err
		}
		if opt.SeqStart > len(saved)+1 {
			return fmt.Errorf("%w: next expected message is %d", ErrTranscriptGap, len(saved)+1)
		}
		overlap := min(len(messages), len(saved)-opt.SeqStart+1)
		for i := 0; i < overlap; i++ {
			old := saved[opt.SeqStart-1+i]
			if old.Seq != messages[i].Seq || old.Role != messages[i].Role || old.Content != messages[i].Content {
				return fmt.Errorf("%w at message %d; save an edited conversation under a new external_session_id", ErrTranscriptConflict, messages[i].Seq)
			}
		}
		fresh := messages[overlap:]
		if len(fresh) > 0 {
			for i := range fresh {
				fresh[i].SessionID = session.ID
			}
			if err := tx.Create(&fresh).Error; err != nil {
				return err
			}
			saved = append(saved, fresh...)
			transcript := make([]TranscriptMessage, 0, len(saved))
			for _, message := range saved {
				transcript = append(transcript, TranscriptMessage{Role: message.Role, Content: message.Content})
			}
			raw, err := json.Marshal(transcript)
			if err != nil {
				return err
			}
			if err := tx.Model(&session).Updates(map[string]any{
				"raw_text": string(raw), "extract_status": types.ExtractPending,
				"extract_error": "", "extracted_at": nil,
			}).Error; err != nil {
				return err
			}
			session.ExtractStatus = types.ExtractPending
		}
		result = ImportResult{
			SessionID: session.ID, MessageCount: len(saved),
			AddedMessages: len(fresh), ExtractStatus: session.ExtractStatus,
		}
		return nil
	})
	return result, err
}
