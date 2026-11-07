package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"recallweave/internal/types"
)

type ReportConfig struct {
	PollInterval     time.Duration
	Timeout          time.Duration
	DeliveryWorkers  int
	SynthesisWorkers int
}

type ReportDispatcher struct {
	agent          *ChatService
	reportStore    ReportStore
	groups         GroupReportStore
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	config         ReportConfig
	workers        sync.WaitGroup
	mu             sync.Mutex
	inFlight       map[uint]bool
	deliverySlots  chan struct{}
	synthesisSlots chan struct{}
	wake           chan struct{}
}

func StartReportDispatcher(ctx context.Context, agent *ChatService, repository ReportStore, config ReportConfig) (*ReportDispatcher, error) {
	if ctx == nil || agent == nil || repository == nil {
		return nil, fmt.Errorf("chat: report dispatcher dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.PollInterval == 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	if config.Timeout == 0 {
		config.Timeout = 45 * time.Second
	}
	if config.DeliveryWorkers == 0 {
		config.DeliveryWorkers = 2
	}
	if config.SynthesisWorkers == 0 {
		config.SynthesisWorkers = 2
	}
	if config.PollInterval < time.Millisecond || config.PollInterval > time.Minute || config.Timeout <= 0 || config.Timeout > 5*time.Minute {
		return nil, fmt.Errorf("chat: invalid report polling interval or timeout")
	}
	if config.DeliveryWorkers < 1 || config.DeliveryWorkers > 32 || config.SynthesisWorkers < 1 || config.SynthesisWorkers > 32 {
		return nil, fmt.Errorf("chat: report worker counts must be between 1 and 32")
	}
	if err := repository.InterruptProcessing(ctx); err != nil {
		return nil, fmt.Errorf("chat: recover report states: %w", err)
	}
	groups, _ := repository.(GroupReportStore)
	if groups != nil {
		if err := groups.InterruptGroupProcessing(ctx); err != nil {
			return nil, fmt.Errorf("chat: recover group report states: %w", err)
		}
	}
	workerCtx, cancel := context.WithCancel(ctx)
	d := &ReportDispatcher{
		agent:       agent,
		reportStore: repository,
		groups:      groups,
		ctx:         workerCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
		config:      config,
	}
	d.inFlight = make(map[uint]bool)
	d.deliverySlots = make(chan struct{}, config.DeliveryWorkers)
	d.synthesisSlots = make(chan struct{}, config.SynthesisWorkers)
	d.wake = make(chan struct{}, 1)
	go d.loop()
	return d, nil
}

func (d *ReportDispatcher) Close(ctx context.Context) error {
	d.cancel()
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *ReportDispatcher) loop() {
	defer close(d.done)
	defer d.workers.Wait()
	ticker := time.NewTicker(d.config.PollInterval)
	defer ticker.Stop()
	for {
		if d.ctx.Err() != nil {
			return
		}
		d.scan()
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		case <-d.wake:
		}
	}
}

func (d *ReportDispatcher) scan() {
	for _, synthesize := range []bool{false, true} {
		d.scanReports(synthesize)
		d.scanGroups(synthesize)
	}
}

func (d *ReportDispatcher) scanReports(synthesize bool) {
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
	defer cancel()
	excluded := d.busyConversations()
	for ctx.Err() == nil && d.hasCapacity(synthesize) {
		rows, err := d.pendingReports(ctx, synthesize, excluded)
		if err != nil {
			if d.ctx.Err() == nil {
				slog.Warn("load pending reports failed", "error", err)
			}
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, row := range rows {
			excluded = append(excluded, row.ConversationID)
			if !d.hasCapacity(synthesize) || ctx.Err() != nil {
				return
			}
			row := row
			d.startDelivery(row.ConversationID, synthesize, func(release func()) { d.process(row, release) })
		}
	}
}

func reportClaimID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (d *ReportDispatcher) process(row types.SubagentTask, release func()) {
	defer release()
	claim, err := reportClaimID()
	if err != nil {
		slog.Error("create report claim failed", "error", err)
		return
	}
	claimed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("automatic report panicked", "task_id", row.ID)
			if claimed {
				d.fail(row, claim, "自动回报异常中断，未自动重试。", nil)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
	task, ok, err := d.reportStore.Claim(ctx, row.ID, row.ConversationID, claim)
	cancel()
	if err != nil {
		if d.ctx.Err() == nil {
			slog.Warn("claim pending report failed", "task_id", row.ID, "error", err)
		}
		// A commit acknowledgement can be lost. Only this matching claim may
		// be failed; a rolled-back or differently claimed task is untouched.
		if d.ctx.Err() == nil || requiresSynthesis(row.DeliveryMode) {
			d.fail(row, claim, "领取回报时无法确认存储状态，未自动重新生成。", nil)
		}
		return
	}
	if !ok {
		return
	}
	claimed = true
	if task.ID != row.ID || task.ConversationID != row.ConversationID {
		d.fail(row, claim, "回报任务所属对话不匹配。", nil)
		return
	}
	ctx, cancel = context.WithTimeout(d.ctx, d.config.Timeout)
	generated, err := d.agent.GenerateReport(ctx, task)
	cancel()
	if d.ctx.Err() != nil {
		if requiresSynthesis(task.DeliveryMode) {
			d.fail(row, claim, "服务停止，自动回报中断；未自动重试模型。", generated.Run)
		}
		return
	}
	if err != nil {
		d.fail(row, claim, "自动回报失败："+err.Error(), generated.Run)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	err = d.reportStore.Deliver(ctx, row.ID, row.ConversationID, claim, generated)
	cancel()
	if err != nil {
		slog.Error("persist automatic report failed", "task_id", row.ID, "error", err)
		d.fail(row, claim, "自动回报保存失败："+err.Error(), generated.Run)
	}
}

func (d *ReportDispatcher) fail(row types.SubagentTask, claim, message string, run *types.AgentRun) {
	if len(message) > 2048 {
		end := 2048
		for end > 0 && !utf8.RuneStart(message[end]) {
			end--
		}
		message = message[:end]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.reportStore.Fail(ctx, row.ID, row.ConversationID, claim, message, run); err != nil {
		slog.Error("save automatic report failure failed", "task_id", row.ID, "error", err)
	}
}
