//go:build embedcli

package cli

import _ "embed"

// Release builds embed the fmsg-cli binary for the target platform (the
// release workflow places it at embedded/fmsg-bin before building with
// -tags embedcli), so users download a single file.
//
//go:embed embedded/fmsg-bin
var embeddedCLI []byte
