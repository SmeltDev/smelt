// Package kit contains the canonical agent kit build inputs.
package kit

import "embed"

// Files contains the Dockerfile and all its local COPY inputs.
//
//go:embed Dockerfile runtime.sh
var Files embed.FS
