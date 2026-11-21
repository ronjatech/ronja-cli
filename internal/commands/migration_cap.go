package commands

// maxPendingPerPush is the server's tools.MaxMigrationsPerSet, pinned there by
// TestCLICarriesTheMigrationCap, which reads this file: a push with more
// pending migrations is refused whole, so neither the status line nor propose
// offers one.
const maxPendingPerPush = 40
