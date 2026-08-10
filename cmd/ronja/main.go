// Command ronja is the Ronja command-line interface.
//
// The main package lives at cmd/ronja/ rather than the module root so that the
// installed binary is named `ronja`: `go install` takes the binary name from
// the last element of the package path, and the module is
// github.com/ronjatech/ronja-cli (the root path would install `ronja-cli`).
//
//	go install github.com/ronjatech/ronja-cli/cmd/ronja@latest
//
// This is also a separate Go module from the backend on purpose: the backend
// module pulls in pgx, Kafka and the AWS SDK, none of which a CLI has any
// business linking against.
package main

import "github.com/ronjatech/ronja-cli/internal/commands"

func main() {
	commands.Execute()
}
