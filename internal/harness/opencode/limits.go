package opencode

import (
	"context"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
)

func (d *Driver) ReadAccountLimits(ctx context.Context) (accountlimits.Snapshot, error) {
	raw, err := d.ReadConsoleLimits(ctx)
	if err != nil {
		return accountlimits.Snapshot{}, err
	}
	return accountlimits.ParseConsole(raw)
}
func (*Driver) AccountLimitsEvents() <-chan struct{} { return nil }
func (*Driver) AccountLimitsDirty() bool             { return false }
