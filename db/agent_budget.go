package db

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/Alexander4731/Vestrail/kernel"
)

func ValidateAgentBudget(p kernel.BudgetPolicy) error {
	for _, seconds := range []*int{&p.SettleSeconds, p.HardBufferSeconds} {
		if seconds != nil && (*seconds < 0 || int64(*seconds) > math.MaxInt64/int64(time.Second)) {
			return fmt.Errorf("收尾时间和硬宽限必须是有效的非负秒数")
		}
	}
	return nil
}

// SetAgentRunBudget commits the run duration and its phase policy together.
func (d *DB) SetAgentRunBudget(key string, runSeconds *int, policy *kernel.BudgetPolicy) error {
	if runSeconds != nil && (*runSeconds < 0 || int64(*runSeconds) > math.MaxInt32) {
		return fmt.Errorf("run_seconds 必须在 0 到 %d 之间", math.MaxInt32)
	}
	if policy != nil {
		if err := ValidateAgentBudget(*policy); err != nil {
			return err
		}
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if runSeconds != nil {
		result, err := tx.Exec(`UPDATE agents SET run_seconds=$2 WHERE key=$1`, key, *runSeconds)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("agent %s 不存在", key)
		}
	}
	if policy != nil {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE key=$1)`, key).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("agent %s 不存在", key)
		}
		raw, err := json.Marshal(policy)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES($1,$2)
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, "agent_budget."+key, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
