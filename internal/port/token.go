package port

// TokenCounter estimates the token length of a piece of text. Implementations
// are heuristic; they exist to drive compression-threshold decisions, not
// billing (billing uses InferenceResponse.PromptTokens from the backend).
type TokenCounter interface {
	Count(text string) int
}
