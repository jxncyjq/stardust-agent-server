package evolution

import (
	"strings"

	"github.com/stardust/legion-agent/internal/memory"
)

type GeneSixTuple struct {
	M     string
	U     string
	Pi    string
	Alpha string
	C     string
	V     string
}

func GeneSixTupleFromMemory(gene memory.Gene) GeneSixTuple {
	return GeneSixTuple{
		M:     gene.Match,
		U:     gene.UseWhen,
		Pi:    gene.Plan,
		Alpha: gene.Avoid,
		C:     gene.Constraints,
		V:     gene.Validation,
	}
}

func ContentAddressedGeneVersion(tuple GeneSixTuple) string {
	canonical := strings.Join([]string{
		tuple.M,
		tuple.U,
		tuple.Pi,
		tuple.Alpha,
		tuple.C,
		tuple.V,
	}, "\x00")
	return "gene-v1-" + contentHash(canonical)[:16]
}
