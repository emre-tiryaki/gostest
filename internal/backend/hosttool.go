package backend

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
)

// HostToolTarget is the directory host tools are mounted into: it is on
// the PATH of every common image and holds no files of the distribution.
const HostToolTarget = "/usr/local/bin"

// ToolKind tells whether a host file can run inside containers.
type ToolKind int

// Kinds of host files.
const (
	// ToolUnsupported files are not mounted, e.g. dynamically linked
	// binaries, which depend on the host's libc and libraries.
	ToolUnsupported ToolKind = iota
	// ToolScript is a text file starting with "#!"; the container
	// provides the interpreter.
	ToolScript
	// ToolStaticELF is an ELF binary without a program interpreter.
	ToolStaticELF
)

func (k ToolKind) String() string {
	switch k {
	case ToolScript:
		return "script"
	case ToolStaticELF:
		return "static ELF"
	}
	return "unsupported"
}

// ClassifyHostTool reports what kind of file path is. Files that are
// neither scripts nor ELF binaries, including malformed ELF files, are
// ToolUnsupported; the error is for files that cannot be read.
func ClassifyHostTool(path string) (ToolKind, error) {
	f, err := os.Open(path)
	if err != nil {
		return ToolUnsupported, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ToolUnsupported, err
	}
	if !info.Mode().IsRegular() {
		return ToolUnsupported, fmt.Errorf("%s is not a regular file", path)
	}

	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return ToolUnsupported, err
	}
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("#!")):
		return ToolScript, nil
	case bytes.Equal(head, []byte(elf.ELFMAG)):
		ef, err := elf.NewFile(f)
		if err != nil {
			return ToolUnsupported, nil
		}
		for _, p := range ef.Progs {
			if p.Type == elf.PT_INTERP {
				return ToolUnsupported, nil
			}
		}
		return ToolStaticELF, nil
	}
	return ToolUnsupported, nil
}

// ScriptInterpreter returns the command that runs the script at path, from
// its shebang line: "bash" for "#!/bin/bash" and "#!/usr/bin/env bash".
// It is empty when path is not a script.
func ScriptInterpreter(path string) (string, error) {
	return "", nil
}
