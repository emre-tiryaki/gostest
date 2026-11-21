// Package deps finds the external commands a shell command or script needs
// without running it, so they can be installed before the first run.
package deps

// MaxFileSize is the largest script Scan parses; of larger files only the
// shebang line is used.
const MaxFileSize = 1 << 20

// Commands returns the external commands script runs, in order of first
// appearance and without duplicates. Shell builtins and keywords, functions
// and aliases the script defines, paths and names only known at run time
// are left out. Arguments of command -v, type, hash and which count too:
// they are the commands a script checks for.
func Commands(script string) []string {
	return nil
}

// Scan returns Commands of command and, recursively, of the shell scripts
// it runs or sources by path (relative paths are resolved against dir),
// plus the interpreters in the shebang lines of the files it executes.
func Scan(command, dir string) []string {
	return Commands(command)
}
