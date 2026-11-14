package types

// TranscriptSource keeps an external conversation attached to one imported session.
// A nil SessionID exists only while the first import transaction is in progress.
type TranscriptSource struct {
	SourceTag         SourceTag `gorm:"primaryKey;size:32"`
	ExternalSessionID string    `gorm:"primaryKey;size:191"`
	SessionID         *uint     `gorm:"uniqueIndex"`
}
