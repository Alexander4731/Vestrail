package kernel

import "time"

// TerminationCode names the authoritative reason an agent run stopped.
type TerminationCode string

const (
	TermCompleted           TerminationCode = "completed"
	TermBudgetRun           TerminationCode = "budget_run"            // 角色自身软/硬时长或 MaxTurns 耗尽
	TermBudgetTask          TerminationCode = "budget_task"           // 任务总 deadline 耗尽
	TermNoProgress          TerminationCode = "no_progress"           // 停滞守卫交回
	TermRepeatedToolFailure TerminationCode = "repeated_tool_failure" // 同一工具连续失败交回
	TermKilledByPlanner     TerminationCode = "killed_by_planner"     // Planner 主动 kill_work
	TermPausedByUser        TerminationCode = "paused_by_user"        // 人工暂停任务/工作
	TermCancelledByUser     TerminationCode = "cancelled_by_user"     // 人工删除任务/工作
	TermGoalMet             TerminationCode = "goal_met"              // 目标已达成终止剩余工作
	TermModelError          TerminationCode = "model_error"           // 真正的模型/网络传输失败
	TermShutdown            TerminationCode = "shutdown"              // 进程关闭
)

const (
	DefaultHardBuffer  = 15 * time.Second
	MinSettleReserve   = 20 * time.Second
	MaxSettleReserve   = 90 * time.Second
	MinShortRunReserve = 5 * time.Second
	ShortRunThreshold  = 30 * time.Second
)

// BudgetPlan splits a run's wall-clock budget into an exploration window,
// a bounded settlement window, and a final hard backstop buffer.
type BudgetPlan struct {
	Total         time.Duration // 0 = unlimited
	Explore       time.Duration // soft explore deadline; 0 = unlimited
	SettleReserve time.Duration // bounded wrap-up window after explore ends
	HardBuffer    time.Duration // final hard-kill cushion after Total
	ClampedByTask bool
	TaskExpired   bool
}

type BudgetPolicy struct {
	SettleSeconds     int  `json:"settle_seconds"`
	HardBufferSeconds *int `json:"hard_buffer_seconds"`
}

// ComputeBudgetPlan derives a unified two-phase budget for Worker, Planner,
// and MainAgent. ownSeconds <= 0 means the agent itself has no time cap
// (never silently rewritten to 5 minutes).
func ComputeBudgetPlan(ownSeconds int, taskDeadlineUnix int64, now time.Time) BudgetPlan {
	return ComputeBudgetPlanWithPolicy(ownSeconds, taskDeadlineUnix, now, BudgetPolicy{})
}

func ComputeBudgetPlanWithPolicy(ownSeconds int, taskDeadlineUnix int64, now time.Time, policy BudgetPolicy) BudgetPlan {
	hard := DefaultHardBuffer
	if policy.HardBufferSeconds != nil {
		hard = time.Duration(max(0, *policy.HardBufferSeconds)) * time.Second
	}
	var own time.Duration
	if ownSeconds > 0 {
		own = time.Duration(ownSeconds) * time.Second
	}
	var total time.Duration
	var clamped bool
	if taskDeadlineUnix > 0 {
		rem := time.Unix(taskDeadlineUnix, 0).Sub(now)
		if rem <= 0 {
			return BudgetPlan{TaskExpired: true, ClampedByTask: true}
		}
		if own <= 0 || rem <= own+hard {
			usable := max(time.Duration(0), rem-hard)
			if usable == 0 {
				return BudgetPlan{TaskExpired: true, ClampedByTask: true, HardBuffer: min(hard, rem)}
			}
			total = usable
			clamped = true
		} else {
			total = own
		}
	} else {
		total = own
	}
	if total <= 0 {
		reserve := MinSettleReserve
		if policy.SettleSeconds > 0 {
			reserve = time.Duration(policy.SettleSeconds) * time.Second
		}
		return BudgetPlan{SettleReserve: reserve, HardBuffer: hard}
	}
	var reserve time.Duration
	if policy.SettleSeconds > 0 {
		reserve = min(time.Duration(policy.SettleSeconds)*time.Second, total/2)
	} else if total <= ShortRunThreshold {
		reserve = total / 3
		if reserve < MinShortRunReserve && total > MinShortRunReserve {
			reserve = MinShortRunReserve
		}
		if reserve >= total {
			reserve = total / 2
		}
	} else {
		reserve = total / 5
		if reserve < MinSettleReserve {
			reserve = MinSettleReserve
		}
		if reserve > MaxSettleReserve {
			reserve = MaxSettleReserve
		}
	}
	return BudgetPlan{
		Total:         total,
		Explore:       total - reserve,
		SettleReserve: reserve,
		HardBuffer:    hard,
		ClampedByTask: clamped,
	}
}

// HardDuration returns the total duration until the hard backstop fires.
func (p BudgetPlan) HardDuration() time.Duration {
	if p.Total <= 0 {
		return 0
	}
	return p.Total + p.HardBuffer
}

// IsBudgetTerm reports whether code represents a local budget exhaustion rather
// than a transient provider error.
func (c TerminationCode) IsBudgetTerm() bool {
	return c == TermBudgetRun || c == TermBudgetTask
}
