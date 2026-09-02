//go:build live

package pi

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
)

func TestAssessmentLiveUsesTheConfirmationTool(t *testing.T) {
	if os.Getenv("PI_ASSESSMENT_LIVE") != "1" {
		t.Skip("set PI_ASSESSMENT_LIVE=1 to run the real Pi assessment probe")
	}
	confirmed := make(chan assessment.ConfirmationBatch, 1)
	adapter := New(func(
		_ context.Context,
		batch assessment.ConfirmationBatch,
	) (assessment.ConfirmationReceipt, error) {
		confirmed <- batch
		receipts := make([]assessment.ConfirmationItemReceipt, len(batch.Results))
		for index, result := range batch.Results {
			receipts[index] = assessment.ConfirmationItemReceipt{
				JobID: result.JobID, AttemptNo: result.AttemptNo,
				Status: assessment.ConfirmationAccepted,
			}
		}
		return assessment.ConfirmationReceipt{Results: receipts}, nil
	})
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	request := assessment.AssessmentRequest{
		TraceID: "0123456789abcdef0123456789abcdef", ResumeVersion: 1,
		Resume: onlineresume.ResumeContent{
			JobIntentions: []onlineresume.JobIntention{{
				Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
			}},
			WorkExperiences:    []string{"三年 Go 后端开发经验"},
			ProjectExperiences: []string{"使用 Go 和 SQLite 构建本地服务"},
			Educations:         []string{"计算机本科"},
			Skills:             []string{"Go", "SQLite"},
		},
		Policy: assessment.Policy{
			Version: 1, Name: "live 合成策略", Rules: []string{"只依据输入；明确匹配判为适合"},
		},
		Jobs: []assessment.AssessmentJobInput{{
			JobID: 1, AttemptNo: 1, PlatformJobID: "synthetic-live-job",
			CanonicalURL: "https://example.invalid/synthetic-live-job", JobTitle: "Go 后端工程师",
			CompanyName: "合成测试公司", City: "福州", Salary: "20-30K",
			FullJD: "使用 Go 开发后端服务\n熟悉 Go 与 SQLite",
			JDHash: "synthetic-live-jd-hash",
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if err := adapter.Submit(ctx, request); err != nil {
		t.Fatalf("submit live Pi assessment: %v", err)
	}
	batch := <-confirmed
	if len(batch.Results) != 1 || batch.Results[0].JobID != 1 || batch.Results[0].AttemptNo != 1 {
		t.Fatalf("live Pi confirmation = %#v", batch)
	}
}
