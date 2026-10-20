// Command ronja is the Ronja command-line interface. It lives at cmd/ronja so
// `go install` names the binary `ronja` (the module root would install
// `ronja-cli`), in a module separate from the backend so it never links pgx,
// Kafka or the AWS SDK.
package main

import "github.com/ronjatech/ronja-cli/internal/commands"

func main() {
	commands.Execute()
}
