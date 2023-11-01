// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package art

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("GITHUB_ACTIONS") != "" || os.Getenv("CI") == "true" {
		// Skip CI on GitHub for now
		// TODO: https://github.com/tailscale/tailscale/issues/7866
		os.Exit(0)
	}
	os.Exit(m.Run())
}
