package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Service) GetActiveResumeUse(ctx context.Context) (*ActiveResumeUse, error) {
	row, err := s.queries.GetActiveDiscoveryResumeUse(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query active discovery resume use: %w", err)
	}
	return &ActiveResumeUse{
		DiscoveryRunID: row.DiscoveryRunID,
		ResumeVersion:  int(row.ResumeVersionNo),
	}, nil
}

func (s *Service) GetLatestRun(ctx context.Context) (*RunView, error) {
	row, err := s.queries.GetLatestDiscoveryRun(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query latest discovery run: %w", err)
	}
	jobs, err := s.pool.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	view := &RunView{
		ID:             row.ID,
		ResumeVersion:  int(row.ResumeVersionNo),
		DiscoveredJobs: len(jobs),
		Status:         Status(row.Status),
	}
	if row.ResumeJson == "" {
		return view, nil
	}
	ranges, err := searchRangesFromSavedResume(row.ResumeJson)
	if err != nil {
		return nil, err
	}
	rangeIndex, err := findSearchRange(ranges, row.CurrentRole.String, row.CurrentCity.String)
	if err != nil {
		return nil, err
	}
	completedRanges := rangeIndex
	if Status(row.Status) == StatusCompleted {
		completedRanges = len(ranges)
	}
	view.CompletedRanges = completedRanges
	view.TotalRanges = len(ranges)
	view.ProgressPercent = completedRanges * 100 / len(ranges)
	view.CurrentRange = ranges[rangeIndex]
	view.NextPage = int(row.NextPage.Int64)
	return view, nil
}

func (s *Service) StartAvailability(ctx context.Context) (ActionAvailability, error) {
	current, err := s.resumeVersions.GetCurrent(ctx)
	if err != nil {
		return ActionAvailability{}, err
	}
	if current == nil {
		return ActionAvailability{
			Code:   "online_resume_required",
			Reason: "请先刷新在线简历，再开始岗位发现",
		}, nil
	}
	if len(current.Content.JobIntentions) == 0 {
		return ActionAvailability{
			Code:   "search_range_required",
			Reason: "当前在线简历没有可用搜索范围",
		}, nil
	}
	active, err := s.GetActiveResumeUse(ctx)
	if err != nil {
		return ActionAvailability{}, err
	}
	if active != nil {
		return ActionAvailability{
			Code:   "unfinished_discovery_exists",
			Reason: "请先处理当前未结束的岗位发现运行",
		}, nil
	}
	return ActionAvailability{Allowed: true}, nil
}
