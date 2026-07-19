package adapter

import (
	"context"
	"math"
	"strings"
)

type KeywordEmbeddingProvider struct{}

func (KeywordEmbeddingProvider) Embed(ctx context.Context, text string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vector := make([]float64, 6)
	lower := strings.ToLower(text)
	keywords := []string{"scheduler", "lock", "approval", "memory", "tool", "audit"}
	for idx, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			vector[idx] = 1
		}
	}
	if isZeroVector(vector) {
		for field := range strings.FieldsSeq(lower) {
			bucket := len(field) % len(vector)
			vector[bucket]++
		}
	}
	normalize(vector)
	return vector, nil
}

func isZeroVector(vector []float64) bool {
	for _, value := range vector {
		if value != 0 {
			return false
		}
	}
	return true
}

func normalize(vector []float64) {
	var sum float64
	for _, value := range vector {
		sum += value * value
	}
	if sum == 0 {
		return
	}
	length := math.Sqrt(sum)
	for idx, value := range vector {
		vector[idx] = value / length
	}
}
