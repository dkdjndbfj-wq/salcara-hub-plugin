package launcher

import (
	"context"
	"errors"
)

// startConfirmed is shared by Linux startup and platform-independent transaction
// tests. A Docker image upgrade can supersede a cached Hub, but only after the
// same process/product/version health acceptance required of a web update.
func (s *store) startConfirmed(ctx context.Context, state diskState, runner processRunner) (diskState, error) {
	candidate, imageUpgrade, err := s.preferBootstrap(state)
	if err != nil {
		return state, err
	}
	current, err := s.resolve(candidate.Current)
	if err != nil {
		return state, err
	}
	if err = runner.Start(ctx, current); err != nil {
		if ctx.Err() != nil || candidate.Previous == nil {
			return state, err
		}
		previous, e := s.resolve(*candidate.Previous)
		if e != nil {
			return state, e
		}
		if e = runner.Start(ctx, previous); e != nil {
			return state, errors.New("confirmed Hub and previous signed version are both unhealthy")
		}
		if e = ctx.Err(); e != nil {
			_ = runner.Stop()
			return state, e
		}
		failed := candidate.Current
		candidate.Current, candidate.Previous = *candidate.Previous, &failed
		if failed.Bootstrap {
			candidate.FailedBootstrap = &bootstrapFailure{Version: current.Version, SHA256: current.SHA256}
		}
		candidate.Job = &jobRecord{Status: "failed", Message: "启动版本未通过健康验证，已恢复前一确认程序；未回滚用户数据"}
		if e = s.save(candidate); e != nil {
			_ = runner.Stop()
			return state, errors.New("fallback running state could not be committed; inspect volume")
		}
		return candidate, nil
	}
	if err = ctx.Err(); err != nil {
		_ = runner.Stop()
		return state, err
	}
	if imageUpgrade {
		candidate.FailedBootstrap = nil
		candidate.Job = &jobRecord{Status: "current", Message: "镜像内置 Hub 已通过版本与健康验证；保留前一签名程序及用户数据"}
		if err = s.save(candidate); err != nil {
			_ = runner.Stop()
			return state, errors.New("image upgrade running state could not be committed; inspect volume")
		}
	}
	return candidate, nil
}
