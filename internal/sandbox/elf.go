package sandbox

import (
	"bytes"
	"debug/elf"
)

// isDynamicallyLinked reports whether data (an ELF executable's bytes)
// carries a PT_INTERP program header, i.e. needs a dynamic linker at
// load time. A false negative here just reproduces gVisor's own opaque
// ENOENT ("failed to load /.ate/ate-ish: no such file or directory",
// with no mention of a missing dynamic linker at all) instead of this
// package's clearer error, so correctness matters more than speed: this
// parses the ELF program headers properly rather than guess from a
// string search (an earlier version of this check matched "ld-linux" as
// a substring and misfired on statically linked binaries that still
// embed that string in unrelated debug data).
func isDynamicallyLinked(data []byte) bool {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return false // not an ELF file at all; let exec time report that
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}
