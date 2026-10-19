package store

import (
	"encoding/json"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"testing"
)

func nativePoolSpec() spec.OrganizationSpec {
	sp := orgfixture.Spec()
	seat := sp.Seats["engineer"]
	seat.PersonalMemory = ""
	seat.Teams = nil
	team := sp.Teams["engineering"]
	team.Coordination = &spec.TeamCoordination{Mode: "self_organising"}
	team.Pool = &spec.TeamPool{MinMembers: 1, MaxMembers: 3, Templates: map[string]spec.WorkerTemplate{"engineer": {Seat: &seat, Instructions: "Do engineering work using your own declared authority."}}, IdleTimeoutSeconds: 900, DrainTimeoutSeconds: 300, RetentionSeconds: 86400}
	sp.Teams["engineering"] = team
	sp.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 1000, ExecutionOutputTokens: 100, MaxRequestOutputTokens: 10}
	sp.MessageRoutes["team_work"] = spec.MessageRoute{From: "team:engineering", To: "team:engineering", Bidirectional: true}
	return sp
}
func TestNativePoolSpecIndependentIdentity(t *testing.T) {
	sp := nativePoolSpec()
	before, _ := json.Marshal(sp)
	rows := []poolRow{{PoolMember: runtimeapi.PoolMember{SeatID: "identity", SeatKey: "pool_abc", Team: "engineering", Template: "engineer"}}}
	augmented := poolSpec(sp, rows)
	after, _ := json.Marshal(sp)
	if string(before) != string(after) {
		t.Fatal("mutated declaration")
	}
	sm, err := compile.Compile(augmented, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	member := sm.Seats["pool_abc"]
	if member.PersonalMemory != "private_abc" {
		t.Fatalf("private memory = %s", member.PersonalMemory)
	}
	for _, c := range member.Capabilities {
		if c.Resource == "connection:tracker" {
			t.Fatal("inherited lead authority")
		}
	}
	if _, ok := sp.Seats[member.Key]; ok {
		t.Fatal("runtime participant added to declaration")
	}
	if len(member.SendTo) == 0 {
		t.Fatal("explicit team route did not expand")
	}
}
