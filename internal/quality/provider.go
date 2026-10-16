package quality

// Provider is a way to produce a Contract in the sandbox: a shell script that
// runs from the repository root, and the reading of what it prints.
type Provider struct {
	Script string
	// Parse turns the script's stdout into a contract.
	Parse func(stdout string) (Contract, error)
}

// CommandProvider is a project's own provider: command prints the contract as
// JSON on stdout.
func CommandProvider(command string) Provider {
	return Provider{
		Script: command,
		Parse:  func(stdout string) (Contract, error) { return ParseContract([]byte(stdout)) },
	}
}
