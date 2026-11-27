package main

import "testing"

func TestAdditionalProfileWorkflows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile bool
		extras  []WorkflowConfig
		valid   bool
	}{
		{"additional standalone", true, []WorkflowConfig{{DestinationFile: "pilot.yml"}}, true},
		{"duplicate profile member", true, []WorkflowConfig{{DestinationFile: "checks.yml"}}, false},
		{"duplicate pipeline file", true, []WorkflowConfig{{DestinationFile: "ci.yml"}}, false},
		{"duplicate extra", true, []WorkflowConfig{{DestinationFile: "pilot.yml"}, {DestinationFile: "pilot.yml"}}, false},
		{"missing destination", true, []WorkflowConfig{{}}, false},
		{"nested path", true, []WorkflowConfig{{DestinationFile: "nested/pilot.yml"}}, false},
		{"relative path", true, []WorkflowConfig{{DestinationFile: "./ci.yml"}}, false},
		{"parent path", true, []WorkflowConfig{{DestinationFile: "../outside.yml"}}, false},
		{"backslash", true, []WorkflowConfig{{DestinationFile: `nested\pilot.yml`}}, false},
		{"wrong extension", true, []WorkflowConfig{{DestinationFile: "pilot.txt"}}, false},
		{"obsolete update workflow", true, []WorkflowConfig{{DestinationFile: "update-issue-on-close.yml"}}, false},
		{"obsolete test workflow", true, []WorkflowConfig{{DestinationFile: "test-issue-on-close.yml"}}, false},
		{"yaml extension", true, []WorkflowConfig{{DestinationFile: "pilot.yaml"}}, true},
		{"joins pipeline", true, []WorkflowConfig{{DestinationFile: "pilot.yml", Pipeline: "ci.yml"}}, false},
		{"without profile", false, []WorkflowConfig{{DestinationFile: "pilot.yml"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := ""
			if tc.profile {
				profile = "p"
			}
			c := Config{Profiles: map[string]Profile{"p": {Workflows: []WorkflowConfig{{DestinationFile: "checks.yml", Pipeline: "ci.yml"}}}}, Repositories: []RepoConfig{{Name: "o/r", Profile: profile, AdditionalWorkflows: tc.extras}}}
			err := expandProfiles(&c)
			if (err == nil) != tc.valid {
				t.Fatalf("error %v, want valid=%t", err, tc.valid)
			}
			if tc.valid && (len(c.Repositories[0].Workflows) != 2 || c.Repositories[0].Workflows[1].DestinationFile != tc.extras[0].DestinationFile) {
				t.Fatalf("additional workflow was lost: %+v", c.Repositories[0].Workflows)
			}
		})
	}
}
