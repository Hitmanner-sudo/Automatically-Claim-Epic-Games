// Package winres makes the small object file that gives the Windows program
// its identity: the version information Windows shows for a file (name,
// description, version) and its application manifest. The Go linker picks
// such a file up from the program's directory (rsrc_windows_amd64.syso).
//
// A program without these is anonymous to Windows: Explorer's "Details" tab
// is empty, Task Manager shows the file name only, and everything that
// judges an unsigned program has nothing to go by. Nothing here pretends to
// be anything: the fields say what the program is, that it is unofficial,
// and which build it is.
package winres

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Info is what the resources say.
type Info struct {
	// Version as given to the build, e.g. "0.2.1".
	Version string
	// Name of the program ("EpicAutoClaim") and of its file.
	Name     string
	FileName string
	// Description is shown by Task Manager and Explorer as the file's
	// description; keep it short.
	Description string
	Company     string
	Copyright   string
	Comments    string
}

// Program is what this program says about itself.
func Program(version string) Info {
	return Info{
		Version:     version,
		Name:        "EpicAutoClaim",
		FileName:    "EpicAutoClaim.exe",
		Description: "EpicAutoClaim",
		Company:     "EpicAutoClaim (open-source project)",
		Copyright:   "Unofficial. Not affiliated with or endorsed by Epic Games.",
		Comments:    "Claims the Epic Games Store's free games through your own Epic Games Launcher.",
	}
}

// parts turns "0.2.1" (or "0.1.34", or anything else) into four numbers.
func parts(version string) [4]uint16 {
	var out [4]uint16
	fields := strings.FieldsFunc(version, func(r rune) bool { return r < '0' || r > '9' })
	for i := 0; i < len(fields) && i < 4; i++ {
		n, err := strconv.ParseUint(fields[i], 10, 16)
		if err != nil {
			break
		}
		out[i] = uint16(n)
	}
	return out
}

func dotted(v [4]uint16) string { return fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3]) }

func le16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func le32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

// wide is a string as Windows stores it: UTF-16, with a terminating zero.
func wide(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = le16(b, u)
	}
	return le16(b, 0)
}

func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// node is one block of a version resource: a length, the length and kind of
// its value, a name, the value, and blocks inside it. Every block starts on
// a four-byte boundary.
type node struct {
	key      string
	value    []byte
	valueLen uint16 // in bytes for binary values, in characters for text
	text     bool
	children []node
}

func (n node) bytes() []byte {
	b := []byte{0, 0} // the length, filled in at the end
	b = le16(b, n.valueLen)
	kind := uint16(0)
	if n.text {
		kind = 1
	}
	b = le16(b, kind)
	b = append(b, wide(n.key)...)
	b = pad4(b)
	b = append(b, n.value...)
	for _, c := range n.children {
		b = pad4(b)
		b = append(b, c.bytes()...)
	}
	binary.LittleEndian.PutUint16(b, uint16(len(b)))
	return b
}

func text(key, value string) node {
	w := wide(value)
	return node{key: key, value: w, valueLen: uint16(len(w) / 2), text: true}
}

// VersionInfo renders the VS_VERSIONINFO resource.
func VersionInfo(in Info) []byte {
	v := parts(in.Version)
	var fixed []byte
	fixed = le32(fixed, 0xFEEF04BD)                       // signature
	fixed = le32(fixed, 0x00010000)                       // structure version
	fixed = le32(fixed, uint32(v[0])<<16|uint32(v[1]))    // file version
	fixed = le32(fixed, uint32(v[2])<<16|uint32(v[3]))    //
	fixed = le32(fixed, uint32(v[0])<<16|uint32(v[1]))    // product version
	fixed = le32(fixed, uint32(v[2])<<16|uint32(v[3]))    //
	fixed = le32(fixed, 0x3F)                             // which flags are valid
	fixed = le32(fixed, 0)                                // flags: none (not a debug or private build)
	fixed = le32(fixed, 0x00040004)                       // for Windows NT
	fixed = le32(fixed, 1)                                // an application
	fixed = le32(fixed, 0)                                // subtype
	fixed = le32(le32(fixed, 0), 0)                       // no date
	const langUSEnglish, codePageUnicode = 0x0409, 0x04B0 // "040904B0"
	strs := node{key: "040904B0", text: true, children: []node{
		text("Comments", in.Comments),
		text("CompanyName", in.Company),
		text("FileDescription", in.Description),
		text("FileVersion", dotted(v)),
		text("InternalName", in.Name),
		text("LegalCopyright", in.Copyright),
		text("OriginalFilename", in.FileName),
		text("ProductName", in.Name),
		text("ProductVersion", in.Version),
	}}
	translation := le16(le16(nil, langUSEnglish), codePageUnicode)
	root := node{key: "VS_VERSION_INFO", value: fixed, valueLen: uint16(len(fixed)), children: []node{
		{key: "StringFileInfo", text: true, children: []node{strs}},
		{key: "VarFileInfo", text: true, children: []node{
			{key: "Translation", value: translation, valueLen: uint16(len(translation))},
		}},
	}}
	return root.bytes()
}

func xmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// Manifest renders the application manifest. It asks for nothing: the
// program runs as the user who starts it, without elevation, and says which
// Windows versions it was made for. (No DPI setting and no dependencies:
// neither is to change how the program behaves.)
func Manifest(in Info) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\r\n" +
		`<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">` + "\r\n" +
		`  <assemblyIdentity type="win32" name="` + xmlText(in.Name) + `" version="` + dotted(parts(in.Version)) + `" processorArchitecture="amd64"/>` + "\r\n" +
		`  <description>` + xmlText(in.Description) + `</description>` + "\r\n" +
		`  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">` + "\r\n" +
		`    <security>` + "\r\n" +
		`      <requestedPrivileges>` + "\r\n" +
		`        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>` + "\r\n" +
		`      </requestedPrivileges>` + "\r\n" +
		`    </security>` + "\r\n" +
		`  </trustInfo>` + "\r\n" +
		`  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">` + "\r\n" +
		`    <application>` + "\r\n" +
		`      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>` + "\r\n" + // Windows 10 and 11
		`      <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>` + "\r\n" + // Windows 8.1
		`      <supportedOS Id="{4a2f28e3-53b9-4441-ba9c-d69d4a4a6e38}"/>` + "\r\n" + // Windows 8
		`      <supportedOS Id="{35138b9a-5d96-4fbd-8e2d-a2440225f93a}"/>` + "\r\n" + // Windows 7
		`    </application>` + "\r\n" +
		`  </compatibility>` + "\r\n" +
		`</assembly>` + "\r\n")
}

// Resource types and the language the resources are filed under.
const (
	rtVersion  = 16
	rtManifest = 24
	langEnUS   = 0x0409
)

// Object renders the object file (COFF, for 64-bit x86) that carries the
// two resources in a section named ".rsrc", which is where the Go linker
// looks for them.
//
// A resource section is a small tree: by type, then by name, then by
// language, ending in an entry that says where the data is. Those "where"
// fields are addresses in the finished program, which only the linker
// knows; each is marked with a relocation and holds, until then, the data's
// place within this section.
func Object(in Info) []byte {
	version, manifest := VersionInfo(in), Manifest(in)

	const (
		dirSize   = 16
		entrySize = 8
		leafSize  = 16
		subdir    = 0x80000000 // "this entry leads to another directory"
	)
	// Where everything sits within the section.
	root := 0
	typeVersion := root + dirSize + 2*entrySize
	typeManifest := typeVersion + dirSize + entrySize
	langVersion := typeManifest + dirSize + entrySize
	langManifest := langVersion + dirSize + entrySize
	leafVersion := langManifest + dirSize + entrySize
	leafManifest := leafVersion + leafSize
	dataVersion := leafManifest + leafSize
	dataManifest := dataVersion + (len(version)+7)&^7
	size := dataManifest + (len(manifest)+7)&^7

	sect := make([]byte, 0, size)
	dir := func(entries ...[2]uint32) {
		sect = le32(sect, 0)                    // characteristics
		sect = le32(sect, 0)                    // time stamp
		sect = le16(le16(sect, 0), 0)           // version
		sect = le16(sect, 0)                    // entries with a name: none
		sect = le16(sect, uint16(len(entries))) // entries with a number
		for _, e := range entries {
			sect = le32(le32(sect, e[0]), e[1])
		}
	}
	dir([2]uint32{rtVersion, subdir | uint32(typeVersion)}, [2]uint32{rtManifest, subdir | uint32(typeManifest)})
	dir([2]uint32{1, subdir | uint32(langVersion)})
	dir([2]uint32{1, subdir | uint32(langManifest)})
	dir([2]uint32{langEnUS, uint32(leafVersion)})
	dir([2]uint32{langEnUS, uint32(leafManifest)})
	leaf := func(at, n int) {
		sect = le32(sect, uint32(at)) // where the data is (see the relocations)
		sect = le32(sect, uint32(n))
		sect = le32(sect, 0) // code page
		sect = le32(sect, 0)
	}
	leaf(dataVersion, len(version))
	leaf(dataManifest, len(manifest))
	sect = append(sect, version...)
	for len(sect) < dataManifest {
		sect = append(sect, 0)
	}
	sect = append(sect, manifest...)
	for len(sect) < size {
		sect = append(sect, 0)
	}

	const (
		fileHeader    = 20
		sectionHeader = 40
		relocSize     = 10
		machineAMD64  = 0x8664
		relAddr32NB   = 3 // "the address of this, relative to the start of the program image"
	)
	relocs := []int{leafVersion, leafManifest}
	rawAt := fileHeader + sectionHeader
	relocAt := rawAt + len(sect)
	symbolsAt := relocAt + len(relocs)*relocSize

	var out []byte
	// File header.
	out = le16(out, machineAMD64)
	out = le16(out, 1)                 // one section
	out = le32(out, 0)                 // no time stamp: the same input gives the same file
	out = le32(out, uint32(symbolsAt)) // symbol table
	out = le32(out, 1)                 // one symbol
	out = le16(out, 0)                 // no optional header
	out = le16(out, 0x0004)            // no line numbers
	// Section header.
	out = append(out, '.', 'r', 's', 'r', 'c', 0, 0, 0)
	out = le32(le32(out, 0), 0) // size and address in memory: the linker's business
	out = le32(out, uint32(len(sect)))
	out = le32(out, uint32(rawAt))
	out = le32(out, uint32(relocAt))
	out = le32(out, 0) // no line numbers
	out = le16(out, uint16(len(relocs)))
	out = le16(out, 0)
	out = le32(out, 0x40000040) // initialised data, readable
	out = append(out, sect...)
	for _, at := range relocs {
		out = le32(out, uint32(at)) // the field to fix
		out = le32(out, 0)          // against symbol 0: the section itself
		out = le16(out, relAddr32NB)
	}
	// Symbol table: the section.
	out = append(out, '.', 'r', 's', 'r', 'c', 0, 0, 0)
	out = le32(out, 0) // value
	out = le16(out, 1) // section 1
	out = le16(out, 0) // no type
	out = append(out, 3, 0)
	// String table: empty (its own four bytes of length).
	out = le32(out, 4)
	return out
}
