package delivery

import "fmt"

const (
	Direct     = "direct"
	Combined   = "combined"
	Synthesize = "synthesize"
)

func NormalizeTaskMode(mode string) (string, error) {
	switch mode {
	case "", Direct:
		return Direct, nil
	case Synthesize:
		return Synthesize, nil
	default:
		return "", fmt.Errorf("delivery: task mode must be direct or synthesize")
	}
}

func NormalizeNewGroupMode(mode string) (string, error) {
	if mode == "" {
		return Direct, nil
	}
	switch mode {
	case Direct, Combined, Synthesize:
		return mode, nil
	default:
		return "", fmt.Errorf("delivery: group mode must be direct, combined or synthesize")
	}
}

// Existing groups were sealed for one final message and have grouped members.
// Keep that delivery shape without adding a model call when their old row is blank.
func ResolveStoredGroupMode(mode string) (string, error) {
	if mode == "" {
		return Combined, nil
	}
	return NormalizeNewGroupMode(mode)
}
