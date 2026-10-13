package compile_test

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"testing"
)

func TestModelBudgetValidation(t *testing.T) {
	for _, tc := range []struct {
		b     spec.ModelBudgets
		valid bool
	}{
		{spec.ModelBudgets{}, false},
		{spec.ModelBudgets{SeatDailyOutputTokens: -1}, false},
		{spec.ModelBudgets{SeatDailyOutputTokens: 1, MaxRequestOutputTokens: -1}, false},
		{spec.ModelBudgets{OrganizationDailyOutputTokens: 1_000_000_000_001}, false},
		{spec.ModelBudgets{MaxRequestOutputTokens: 100}, false},
		{spec.ModelBudgets{SeatDailyOutputTokens: 10}, true},
		{spec.ModelBudgets{OrganizationDailyOutputTokens: 100, ExecutionOutputTokens: 20, MaxRequestOutputTokens: 10}, true},
	} {
		sp := compileValid(t).Spec
		sp.ModelBudgets = &tc.b
		m, err := compile.Compile(sp, compile.DefaultCatalog())
		if (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc.b, err)
		}
		if err == nil && *m.Spec.ModelBudgets != tc.b {
			t.Fatal("budget not retained in manifest")
		}
	}
}
