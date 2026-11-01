package main

import "testing"

// A caller's permissions block caps every job in the reusable workflow it
// calls, and any scope it omits becomes none. Every reusable workflow checks
// out the repository, so a block without contents makes GitHub reject the run
// at startup with no job and no log.
func TestConfiguredPermissionsGrantContents(t *testing.T) {
	cfg := loadRealConfig(t)
	for _, repo := range cfg.Repositories {
		for _, wf := range repo.Workflows {
			if len(wf.Permissions) == 0 {
				continue
			}
			if got := wf.Permissions["contents"]; got != "read" && got != "write" {
				t.Errorf("%s %s: permissions %v omit contents", repo.Name, wf.DestinationFile, wf.Permissions)
			}
		}
	}
}
