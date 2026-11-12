package winres

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"0.2.1": "0.2.1.0", "0.1.34": "0.1.34.0", "1.2.3.4": "1.2.3.4", "v2.0": "2.0.0.0",
		"cef90": "90.0.0.0", "dev": "0.0.0.0", "": "0.0.0.0", "1.2.3.4.5": "0.0.0.0", "70000.1": "0.0.0.0", "0.2.20261112": "0.0.0.0",
	} {
		if got := dotted(parts(in)); got != want {
			t.Errorf("version %q -> %s, want %s", in, got, want)
		}
	}
	// A version that is to be published must be one Windows can hold.
	for in, ok := range map[string]bool{"0.2.1": true, "0.1.34": true, "1.2.3.4": true, "65535.0.1": true, "0.2.20261112": false, "1.70000.5": false, "v1.2.3": false, "dev": false, "": false, "1.2.3.4.5": false} {
		if got := Check(in) == nil; got != ok {
			t.Errorf("Check(%q) ok=%v, want %v", in, got, ok)
		}
	}
}

// block is one block of a version resource as Windows walks it.
type block struct {
	key      string
	valueLen int
	text     bool
	value    []byte
	children []block
}

// walk reads the block at the start of b (which ends where its length
// says), the way VerQueryValue does: the name, padding to a multiple of
// four, the value, padding, then the blocks inside it one after another.
func walk(t *testing.T, b []byte, base int) block {
	t.Helper()
	length := int(binary.LittleEndian.Uint16(b))
	if length > len(b) || length < 6 {
		t.Fatalf("block at %d: length %d of %d", base, length, len(b))
	}
	blk := block{valueLen: int(binary.LittleEndian.Uint16(b[2:])), text: binary.LittleEndian.Uint16(b[4:]) == 1}
	at := 6
	var name []uint16
	for {
		u := binary.LittleEndian.Uint16(b[at:])
		at += 2
		if u == 0 {
			break
		}
		name = append(name, u)
	}
	blk.key = string(utf16Decode(name))
	align := func(n int) int { return (base+n+3)&^3 - base }
	at = align(at)
	valueBytes := blk.valueLen
	if blk.text {
		valueBytes *= 2
	}
	if at+valueBytes > length {
		t.Fatalf("block %q: its value (%d bytes at %d) runs past its end (%d)", blk.key, valueBytes, at, length)
	}
	blk.value = b[at : at+valueBytes]
	at = align(at + valueBytes)
	for at < length {
		child := walk(t, b[at:length], base+at)
		blk.children = append(blk.children, child)
		at = align(at + int(binary.LittleEndian.Uint16(b[at:])))
	}
	if at != align(length) {
		t.Fatalf("block %q: what is inside it ends at %d, its length says %d", blk.key, at, length)
	}
	return blk
}

func utf16Decode(u []uint16) []rune {
	out := make([]rune, len(u))
	for i, c := range u {
		out[i] = rune(c) // (the names and values here are plain letters)
	}
	return out
}

func (b block) child(t *testing.T, key string) block {
	t.Helper()
	for _, c := range b.children {
		if c.key == key {
			return c
		}
	}
	t.Fatalf("no %q inside %q", key, b.key)
	return block{}
}

func TestVersionInfoCanBeWalkedTheWayWindowsWalksIt(t *testing.T) {
	for _, version := range []string{"0.2.1", "0.1.345", "10.20.30", "1.0"} {
		in := Program(version)
		raw := VersionInfo(in)
		root := walk(t, raw, 0)
		if root.key != "VS_VERSION_INFO" || root.text || root.valueLen != 52 || len(root.children) != 2 {
			t.Fatalf("root: %q, %d children", root.key, len(root.children))
		}
		tr := root.child(t, "VarFileInfo").child(t, "Translation")
		if tr.text || tr.valueLen != 4 || !bytes.Equal(tr.value, []byte{0x09, 0x04, 0xB0, 0x04}) {
			t.Errorf("Translation: %+v", tr)
		}
		table := root.child(t, "StringFileInfo").child(t, "040904B0")
		want := map[string]string{
			"Comments": in.Comments, "CompanyName": in.Company, "FileDescription": in.Description,
			"FileVersion": dotted(parts(version)), "InternalName": in.Name, "LegalCopyright": in.Copyright,
			"OriginalFilename": in.FileName, "ProductName": in.Name, "ProductVersion": version,
		}
		if len(table.children) != len(want) {
			t.Errorf("version %s: %d strings, want %d", version, len(table.children), len(want))
		}
		for key, text := range want {
			s := table.child(t, key)
			// The length of a string counts its characters and the zero
			// that ends it.
			if !s.text || s.valueLen != len([]rune(text))+1 || !bytes.Equal(s.value, wide(text)) {
				t.Errorf("version %s: %s = %q (length %d), want %q", version, key, s.value, s.valueLen, text)
			}
		}
	}
}

func TestVersionInfoIsLaidOutAsWindowsReadsIt(t *testing.T) {
	v := VersionInfo(Program("0.2.1"))
	// The first two bytes are the length of the whole thing.
	if n := int(binary.LittleEndian.Uint16(v)); n != len(v) || len(v)%2 != 0 {
		t.Fatalf("length field %d, actual %d", n, len(v))
	}
	// Header (6 bytes), "VS_VERSION_INFO" with its zero (32 bytes), two of
	// padding: the fixed part starts at 40 with its signature, and carries
	// the version as four numbers, twice.
	if binary.LittleEndian.Uint16(v[2:]) != 52 || binary.LittleEndian.Uint32(v[40:]) != 0xFEEF04BD {
		t.Fatalf("fixed part: length %d, signature %#x", binary.LittleEndian.Uint16(v[2:]), binary.LittleEndian.Uint32(v[40:]))
	}
	if ms, ls := binary.LittleEndian.Uint32(v[48:]), binary.LittleEndian.Uint32(v[52:]); ms != 0<<16|2 || ls != 1<<16|0 {
		t.Fatalf("file version %#x %#x", ms, ls)
	}
	for _, want := range []string{"StringFileInfo", "040904B0", "ProductName", "EpicAutoClaim", "FileVersion", "0.2.1.0", "ProductVersion", "OriginalFilename", "EpicAutoClaim.exe", "VarFileInfo", "Translation", "Not affiliated with or endorsed by Epic Games."} {
		if !bytes.Contains(v, wide(want)[:len(wide(want))-2]) {
			t.Errorf("%q is missing", want)
		}
	}
	// It says what it is and no more: nobody else's name as the maker.
	for _, not := range []string{"Microsoft", "Epic Games, Inc", "Epic Games Launcher\x00"} {
		if bytes.Contains(v, wide(not)[:len(wide(not))-2]) {
			t.Errorf("the version information names %q", not)
		}
	}
	if m := string(Manifest(Program("0.2.1"))); !strings.Contains(m, `version="0.2.1.0"`) || !strings.Contains(m, `level="asInvoker"`) || strings.Contains(m, "dpiAware") || strings.Contains(m, "dependency") || strings.Count(m, "<supportedOS") != 1 {
		t.Errorf("manifest:\n%s", m)
	}
}

// resource finds the data of a resource (type, name 1, US English) in a
// linked program.
func resource(t *testing.T, f *pe.File, typ uint32) []byte {
	t.Helper()
	oh, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok {
		t.Fatal("not a 64-bit program")
	}
	dd := oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE]
	if dd.VirtualAddress == 0 || dd.Size == 0 {
		t.Fatal("the program has no resources")
	}
	for _, s := range f.Sections {
		if dd.VirtualAddress < s.VirtualAddress || dd.VirtualAddress >= s.VirtualAddress+s.VirtualSize {
			continue
		}
		data, err := s.Data()
		if err != nil {
			t.Fatal(err)
		}
		base := dd.VirtualAddress - s.VirtualAddress
		// find looks an id up in the directory at an offset and returns
		// what its entry points to.
		find := func(dir, id uint32) uint32 {
			n := uint32(binary.LittleEndian.Uint16(data[base+dir+12:])) + uint32(binary.LittleEndian.Uint16(data[base+dir+14:]))
			for i := uint32(0); i < n; i++ {
				e := base + dir + 16 + 8*i
				if binary.LittleEndian.Uint32(data[e:]) == id {
					return binary.LittleEndian.Uint32(data[e+4:])
				}
			}
			t.Fatalf("no entry %d in the directory at %d", id, dir)
			return 0
		}
		byType := find(0, typ)
		byName := find(byType&^0x80000000, 1)
		leaf := find(byName&^0x80000000, langEnUS)
		if byType&0x80000000 == 0 || byName&0x80000000 == 0 || leaf&0x80000000 != 0 {
			t.Fatalf("the tree is not type/name/language/data: %#x %#x %#x", byType, byName, leaf)
		}
		rva, size := binary.LittleEndian.Uint32(data[base+leaf:]), binary.LittleEndian.Uint32(data[base+leaf+4:])
		if rva < s.VirtualAddress || rva+size > s.VirtualAddress+s.VirtualSize {
			t.Fatalf("the data of resource type %d lies outside the resource section: address %#x, size %d, section %#x+%#x", typ, rva, size, s.VirtualAddress, s.VirtualSize)
		}
		return data[rva-s.VirtualAddress : rva-s.VirtualAddress+size]
	}
	t.Fatal("no section holds the resources")
	return nil
}

func TestTheLinkerTakesTheObjectAndTheProgramCarriesTheResources(t *testing.T) {
	// The real test of the object file: a Windows program is linked with
	// it, and the version information and manifest are read back out of
	// the finished program, from where Windows will look for them.
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool to link a program with")
	}
	dir := t.TempDir()
	in := Program("0.2.1")
	files := map[string]string{
		"go.mod":  "module resprobe\n\ngo 1.21\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "rsrc_windows_amd64.syso"), Object(in), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, flags := range []string{"-H=windowsgui -s -w", ""} {
		exe := filepath.Join(dir, "probe.exe")
		cmd := exec.Command(goTool, "build", "-trimpath", "-ldflags", flags, "-o", exe, ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=-mod=mod", "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("linking with the resource object failed (flags %q): %v\n%s", flags, err, out)
		}
		f, err := pe.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		if got := resource(t, f, rtVersion); !bytes.Equal(got, VersionInfo(in)) {
			t.Errorf("flags %q: the version information in the program is not what was put in (%d bytes, want %d)", flags, len(got), len(VersionInfo(in)))
		}
		if got := resource(t, f, rtManifest); !bytes.Equal(got, Manifest(in)) {
			t.Errorf("flags %q: the manifest in the program is not what was put in:\n%s", flags, got)
		}
		f.Close()
	}
}
