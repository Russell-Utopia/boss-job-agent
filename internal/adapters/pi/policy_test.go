package pi

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
)

func TestAdapterImplementsPolicyAdvisorWithoutAssessmentConfirmation(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	executable := writeFakePi(t, directory, `
set -eu
IFS= read -r prompt
case "$prompt" in
  *policy-generate*)
    printf '%s\n' '{"id":"policy-generate","type":"response","command":"prompt","success":true}'
    printf '%s\n' '{"type":"message","message":{"content":[{"text":"明确匹配时通过"}]}}'
    ;;
  *policy-validate*)
    printf '%s\n' '{"id":"policy-validate","type":"response","command":"prompt","success":true}'
    printf '%s\n' '{"type":"result","data":{"results":[{"jobId":7,"currentStatus":"suitable","candidateStatus":"suitable"}]}}'
    ;;
esac
printf '%s\n' '{"type":"agent_end"}'
`)
	adapter := NewWithConfig(Config{Executable: executable, RuntimeDir: filepath.Join(directory, "runtime")}, nil)
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	request := assessment.PolicyGenerationRequest{
		Resume:        onlineresume.ResumeContent{WorkExperiences: []string{"Go 后端开发"}},
		ResumeVersion: 2,
		Policy:        assessment.Policy{Version: 1, Rules: []string{"只依据完整输入"}},
		Samples:       []jobpool.HumanReviewSample{{JobID: 7, Verdict: jobpool.HumanVerdictSuitable}},
	}
	draft, err := adapter.Generate(t.Context(), request)
	if err != nil {
		t.Fatalf("generate policy through fake Pi: %v", err)
	}
	if draft.Text == "" {
		t.Fatal("fake Pi returned an empty policy draft")
	}
	validation, err := adapter.Validate(t.Context(), assessment.PolicyValidationRequest{
		Resume: request.Resume, ResumeVersion: request.ResumeVersion, Policy: request.Policy,
		Candidate: assessment.Policy{Rules: []string{"明确匹配时通过"}}, Samples: request.Samples,
		GenerationSampleIDs: []int64{7},
	})
	if err != nil {
		t.Fatalf("validate policy through fake Pi: %v", err)
	}
	if len(validation.Results) != 1 || validation.Results[0].JobID != 7 {
		t.Fatalf("validation result = %#v", validation.Results)
	}
}
