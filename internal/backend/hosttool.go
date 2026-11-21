package backend

// HostToolTarget is the directory host tools are mounted into: it is on
// the PATH of every common image and holds no files of the distribution.
const HostToolTarget = "/usr/local/bin"

// ToolKind tells whether a host file can run inside containers.
type ToolKind int

// Kinds of host files.
const (
	// ToolUnsupported files are not mounted, e.g. dynamically linked
	// binaries, which depend on the host's libc.
	ToolUnsupported ToolKind = iota
	// ToolScript is a text file starting with "#!"; the container
	// provides the interpreter.
	ToolScript
	// ToolStaticELF is an ELF binary without a program interpreter.
	ToolStaticELF
)

// ClassifyHostTool reports what kind of file path is.
func ClassifyHostTool(path string) (ToolKind, error) {
	return ToolUnsupported, nil
}
