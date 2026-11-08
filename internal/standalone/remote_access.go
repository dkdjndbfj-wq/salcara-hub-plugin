package standalone

import (
	"errors"
	"io"
	"os"
	"strings"

	"salcara/hub/internal/hub"
)

func loadRemoteAccessPolicy(path, override string, explicit bool) (hub.RemoteAccessPolicy, error) {
	policy := hub.RemoteAccessPolicy{Mode: strings.TrimSpace(override)}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if explicit {
			return policy, errors.New("explicit remote source policy file is unavailable")
		}
		return policy, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 32<<10 {
		if explicit {
			return policy, errors.New("remote source policy file must be a small regular file")
		}
		return policy, errors.New("saved remote source policy file is invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return policy, errors.New("remote source policy file is unavailable")
	}
	defer f.Close()
	opened, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		return policy, errors.New("remote source policy file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 32<<10+1))
	if err != nil || len(data) > 32<<10 {
		return policy, errors.New("remote source policy file is invalid")
	}
	parsed, err := hub.ParseRemoteAccessPolicy(data)
	if err != nil {
		return policy, errors.New("cannot parse remote source policy file")
	}
	if strings.TrimSpace(override) != "" {
		// An explicit environment mode is an operator override. It may force
		// disabled/open without discarding issuer trust anchors from disk.
		parsed.Mode = strings.TrimSpace(override)
	}
	return parsed, nil
}
