package runtimeapi

import "testing"

func TestElasticTeamValidation(t *testing.T) {
	valid := func() ElasticTeamRequest {
		return ElasticTeamRequest{Workers: []ElasticWorkerRequest{{Name: "research", Template: "reader", Task: TaskRunRequest{Prompt: "find evidence"}}}}
	}
	req := valid()
	if err := ValidateElasticTeam(&req); err != nil {
		t.Fatal(err)
	}
	if req.Workers[0].Task.TimeoutSeconds != 0 {
		t.Fatal("omitted timeout must remain policy-defaulted")
	}
	for name, change := range map[string]func(*ElasticTeamRequest){
		"unsafe name":      func(r *ElasticTeamRequest) { r.Workers[0].Name = "<bad>" },
		"empty":            func(r *ElasticTeamRequest) { r.Workers = nil },
		"too many":         func(r *ElasticTeamRequest) { r.Workers = make([]ElasticWorkerRequest, 33) },
		"duplicate":        func(r *ElasticTeamRequest) { r.Workers = append(r.Workers, r.Workers[0]) },
		"missing template": func(r *ElasticTeamRequest) { r.Workers[0].Template = "" },
		"forged child id":  func(r *ElasticTeamRequest) { r.Workers[0].Task.RequestID = "a" },
		"forged template":  func(r *ElasticTeamRequest) { r.Workers[0].Task.Template = "a" },
		"nested launch":    func(r *ElasticTeamRequest) { r.Workers[0].Task.ToolNames = []string{"teams.expand"} },
		"write tools":      func(r *ElasticTeamRequest) { r.Workers[0].Task.ToolNames = []string{"memory.write"} },
		"traversal": func(r *ElasticTeamRequest) {
			r.Workers[0].Task.InputFiles = []TaskFile{{Path: "../secret", Content: "a"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := valid()
			change(&r)
			if err := ValidateElasticTeam(&r); err == nil {
				t.Fatal("accepted invalid team")
			}
		})
	}
}
