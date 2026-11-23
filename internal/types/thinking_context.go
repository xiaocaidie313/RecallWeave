package types

// ThoughtSource is the exact range of original messages selected by the user.
type ThoughtSource struct {
	SessionID uint `json:"session_id"`
	SeqStart  int  `json:"seq_start"`
	SeqEnd    int  `json:"seq_end"`
}

// ThinkingContext records the starting point as seen by this run, even if the
// user later replaces the checkpoint. A selected note need not have a Memory.
type ThinkingContext struct {
	Source     *ThoughtSource      `json:"source,omitempty"`
	Checkpoint *ThinkingCheckpoint `json:"checkpoint,omitempty"`
	Navigation bool                `json:"navigation,omitempty"` // The user opened a note or resumed without typing a thought.
}
