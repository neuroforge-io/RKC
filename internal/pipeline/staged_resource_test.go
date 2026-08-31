package pipeline

import "testing"

func TestValueFlowStageDeclaresCompleteWorkingSet(t *testing.T) {
	request := (&stagedScanState{}).stageResources("value-flow")
	if request.MemoryMiB != 512 || request.CPU != 1 || request.Processes != 1 ||
		request.OpenFiles != 128 || request.IOClass != "normal" {
		t.Fatalf("value-flow resource request = %+v", request)
	}
}
