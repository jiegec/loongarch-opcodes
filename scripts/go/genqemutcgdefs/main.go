package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/loongson-community/loongarch-opcodes/scripts/go/common"
)

const attribUnused = "__attribute__((unused))"

//go:embed qemu.clang-format
var qemuStyleFileBytes []byte

func main() {
	// unconditionally take all instruction description files,
	// filtering is done by individually attaching @qemu attribute for
	// insns we want to use
	inputs, err := filepath.Glob("../../*.txt")
	if err != nil {
		panic(err)
	}

	descs, err := common.ReadInsnDescs(inputs)
	if err != nil {
		panic(err)
	}

	descs = filterUnusedInsns(descs)

	formats := gatherFormats(descs)
	scs := gatherDistinctSlotCombinations(formats)

	sort.Slice(descs, func(i int, j int) bool {
		return descs[i].Word < descs[j].Word
	})

	sort.Slice(formats, func(i int, j int) bool {
		return formats[i].CanonicalRepr() < formats[j].CanonicalRepr()
	})

	ectx := common.EmitterCtx{
		DontGofmt: true,
	}

	ectx.Emit("/* SPDX-License-Identifier: MIT */\n")
	ectx.Emit("/*\n")
	ectx.Emit(" * LoongArch instruction formats, opcodes, and encoders for TCG use.\n")
	ectx.Emit(" *\n")
	ectx.Emit(" * This file is auto-generated by genqemutcgdefs from\n")
	ectx.Emit(" * https://github.com/loongson-community/loongarch-opcodes,\n")
	ectx.Emit(" * from commit %s.\n", common.MustGetGitCommitHash())
	ectx.Emit(" * DO NOT EDIT.\n")
	ectx.Emit(" */\n")

	emitOpcEnum(&ectx, descs)

	emitSlotEncoders(&ectx, scs)

	for _, f := range formats {
		emitFmtEncoderFn(&ectx, f)
	}

	for _, d := range descs {
		emitTCGEmitterForInsn(&ectx, d)
	}

	ectx.Emit("\n/* End of generated code.  */\n")

	result := ectx.Finalize()

	// format the generated code with clang-format, using the qemu style
	//
	// due to clang-format madness (can't customize .clang-format path nor filename),
	// we have to use a temporary directory for not polluting our repo with
	// inadequately named file(s)
	//
	// see https://bugs.llvm.org/show_bug.cgi?id=20753
	var formattedResult []byte
	{
		tempdir, err := ioutil.TempDir("", "genqemutcgdefs.*")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(tempdir)

		// write the style file there
		styleFilePath := filepath.Join(tempdir, ".clang-format")
		err = ioutil.WriteFile(styleFilePath, qemuStyleFileBytes, 0644)
		if err != nil {
			panic(err)
		}

		err = os.Chdir(tempdir)
		if err != nil {
			panic(err)
		}

		clangFormat := exec.Command(
			"clang-format",
			"--style=file",
		)
		clangFormat.Stdin = bytes.NewBuffer(result)
		formattedResult, err = clangFormat.Output()
		if err != nil {
			exitError, ok := err.(*exec.ExitError)
			if !ok {
				panic(err)
			}
			fmt.Fprintf(os.Stderr, "fatal: clang-format failed\nstderr:\n%s", string(exitError.Stderr))
			panic(err)
		}
	}

	os.Stdout.Write(formattedResult)
}

////////////////////////////////////////////////////////////////////////////

func filterUnusedInsns(descs []*common.InsnDescription) []*common.InsnDescription {
	var result []*common.InsnDescription
	for _, d := range descs {
		if _, ok := d.Attribs["qemu"]; !ok {
			// QEMU TCG doesn't emit this instruction for now, so ignore this
			// to reduce code size.
			continue
		}

		result = append(result, d)
	}

	return result
}

func gatherFormats(descs []*common.InsnDescription) []*common.InsnFormat {
	formatsSet := make(map[string]*common.InsnFormat)
	for _, d := range descs {
		canonicalFormatName := d.Format.CanonicalRepr()
		if _, ok := formatsSet[canonicalFormatName]; !ok {
			formatsSet[canonicalFormatName] = d.Format
		}
	}

	result := make([]*common.InsnFormat, 0, len(formatsSet))
	for _, f := range formatsSet {
		result = append(result, f)
	}

	return result
}

const (
	slotD = 0
	slotJ = 5
	slotK = 10
	slotA = 15
	slotM = 16
)

func gatherDistinctSlotCombinations(fmts []*common.InsnFormat) []string {
	slotCombinationsSet := make(map[string]struct{})
	for _, f := range fmts {
		// skip EMPTY
		if len(f.Args) == 0 {
			continue
		}
		slotCombinationsSet[slotCombinationForFmt(f)] = struct{}{}
	}

	result := make([]string, 0, len(slotCombinationsSet))
	for sc := range slotCombinationsSet {
		result = append(result, sc)
	}
	sort.Strings(result)

	return result
}

// slot combination looks like "DJKM"
func slotCombinationForFmt(f *common.InsnFormat) string {

	var slots []int
	for _, a := range f.Args {
		for _, s := range a.Slots {
			slots = append(slots, int(s.Offset))
		}
	}
	sort.Ints(slots)

	var sb strings.Builder
	for _, s := range slots {
		switch s {
		case slotD:
			sb.WriteRune('D')
		case slotJ:
			sb.WriteRune('J')
		case slotK:
			sb.WriteRune('K')
		case slotA:
			sb.WriteRune('A')
		case slotM:
			sb.WriteRune('M')
		default:
			panic("should never happen")
		}
	}

	return sb.String()
}

func slotOffsetFromRune(s rune) int {
	switch s {
	case 'D', 'd':
		return slotD
	case 'J', 'j':
		return slotJ
	case 'K', 'k':
		return slotK
	case 'A', 'a':
		return slotA
	case 'M', 'm':
		return slotM
	default:
		panic("should never happen")
	}
}

////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////

// e.g. "amadd_db.w" -> "AMADD_DB_W"
func insnMnemonicToUpperCase(x string) string {
	return strings.ToUpper(strings.ReplaceAll(x, ".", "_"))
}

func insnMnemonicToEnumVariantName(x string) string {
	return fmt.Sprintf("OPC_%s", insnMnemonicToUpperCase(x))
}

func emitOpcEnum(ectx *common.EmitterCtx, descs []*common.InsnDescription) {
	ectx.Emit("\ntypedef enum {\n")

	for _, d := range descs {
		enumVariantName := insnMnemonicToEnumVariantName(d.Mnemonic)

		ectx.Emit(
			"    %s = 0x%08x,\n",
			enumVariantName,
			d.Word,
		)
	}

	ectx.Emit("} LoongArchInsn;\n")
}

func insnFieldNameForRegArg(a *common.Arg) string {
	return strings.ToLower(a.CanonicalRepr())
}

type fieldDesc struct {
	name string
	typ  string
}

func fieldDescsForArgs(args []*common.Arg) []fieldDesc {
	result := make([]fieldDesc, len(args))
	for i, a := range args {
		fieldName := insnFieldNameForRegArg(a)

		var typ string
		switch a.Kind {
		case common.ArgKindIntReg, common.ArgKindFPReg, common.ArgKindFCCReg:
			typ = "TCGReg"
		case common.ArgKindSignedImm:
			typ = "int32_t"
		case common.ArgKindUnsignedImm:
			typ = "uint32_t"
		}

		result[i] = fieldDesc{name: fieldName, typ: typ}
	}

	return result
}

func emitSlotEncoders(ectx *common.EmitterCtx, scs []string) {
	for _, sc := range scs {
		emitSlotEncoderFn(ectx, sc)
	}
}

func slotEncoderFnNameForSc(sc string) string {
	plural := ""
	if len(sc) > 1 {
		plural = "s"
	}

	return fmt.Sprintf("encode_%s_slot%s", strings.ToLower(sc), plural)
}

func emitSlotEncoderFn(ectx *common.EmitterCtx, sc string) {
	funcName := slotEncoderFnNameForSc(sc)
	scLower := strings.ToLower(sc)

	ectx.Emit("\nstatic int32_t %s\n%s(LoongArchInsn opc", attribUnused, funcName)
	for _, s := range scLower {
		ectx.Emit(", uint32_t %c", s)
	}
	ectx.Emit(")\n{\n")

	ectx.Emit("    return opc")

	for _, s := range scLower {
		offset := slotOffsetFromRune(s)

		ectx.Emit(" | %c", s)
		if offset > 0 {
			ectx.Emit(" << %d", offset)
		}
	}

	ectx.Emit(";\n}\n")
}

func fmtEncoderFnNameForInsnFormat(f *common.InsnFormat) string {
	return fmt.Sprintf("encode_%s_insn", strings.ToLower(f.CanonicalRepr()))
}

func emitFmtEncoderFn(ectx *common.EmitterCtx, f *common.InsnFormat) {
	// EMPTY doesn't need encoder after all
	if len(f.Args) == 0 {
		return
	}

	argFieldDescs := fieldDescsForArgs(f.Args)

	ectx.Emit("\nstatic int32_t %s\n%s(LoongArchInsn opc", attribUnused, fmtEncoderFnNameForInsnFormat(f))
	for i := range f.Args {
		ectx.Emit(", %s %s", argFieldDescs[i].typ, argFieldDescs[i].name)
	}
	ectx.Emit(")\n{\n")

	for i, a := range f.Args {
		varName := argFieldDescs[i].name
		ectx.Emit("    tcg_debug_assert(")

		switch a.Kind {
		case common.ArgKindIntReg,
			common.ArgKindFPReg,
			common.ArgKindFCCReg:
			// 0 <= x <= max
			max := (1 << a.TotalWidth()) - 1
			ectx.Emit("%s >= 0 && %s <= 0x%x", varName, varName, max)

		case common.ArgKindSignedImm:
			// -min <= x <= max
			max := (1 << (a.TotalWidth() - 1)) - 1
			negativeMin := max + 1
			ectx.Emit("%s >= -0x%x && %s <= 0x%x", varName, negativeMin, varName, max)

		case common.ArgKindUnsignedImm:
			// x <= max
			max := (1 << a.TotalWidth()) - 1
			ectx.Emit("%s <= 0x%x", varName, max)

		default:
			panic("unreachable")
		}

		ectx.Emit(");\n")
	}

	// collect slot expressions
	slotExprs := make(map[uint]string)
	for argIdx, a := range f.Args {
		argVarName := argFieldDescs[argIdx].name

		if len(a.Slots) == 1 {
			if a.Kind == common.ArgKindSignedImm {
				// signed imms need masking to convert to unsigned slot value
				mask := (1 << a.TotalWidth()) - 1
				slotExprs[a.Slots[0].Offset] = fmt.Sprintf("%s & 0x%x", argVarName, mask)
			} else {
				// and pass through everything else
				slotExprs[a.Slots[0].Offset] = argVarName
			}
		} else {
			// remainingBits is shift amount to extract the current slot from arg
			//
			// take example of Sd5k16:
			//
			// Sd5k16 = (MSB) DDDDDKKKKKKKKKKKKKKKK (LSB)
			//
			// initially remainingBits = 5+16
			//
			// consume from left to right:
			//
			// slot d5: remainingBits = 16
			// thus d5 = (sd5k16 >> 16) & 0b11111
			// emit (d5 expr above)
			//
			// slot k16: remainingBits = 0
			// thus k16 = (sd5k16 >> 0) & 0b1111111111111111
			//          = sd5k16 & 0b1111111111111111
			// emit (k16 expr above)
			remainingBits := int(a.TotalWidth())
			for _, s := range a.Slots {
				remainingBits -= int(s.Width)
				mask := int((1 << s.Width) - 1)

				var sb strings.Builder

				if remainingBits > 0 {
					sb.WriteRune('(')
					sb.WriteString(argVarName)
					sb.WriteString(" >> ")
					sb.WriteString(strconv.Itoa(remainingBits))
					sb.WriteRune(')')
				} else {
					sb.WriteString(argVarName)
				}

				sb.WriteString(" & 0x")
				sb.WriteString(strconv.FormatUint(uint64(mask), 16))

				slotExprs[s.Offset] = sb.String()
			}
		}
	}

	sc := slotCombinationForFmt(f)
	encFnName := slotEncoderFnNameForSc(sc)
	ectx.Emit("    return %s(opc", encFnName)

	for _, s := range sc {
		offset := uint(slotOffsetFromRune(s))
		slotExpr, ok := slotExprs[offset]
		if !ok {
			panic("should never happen")
		}
		ectx.Emit(", %s", slotExpr)
	}

	ectx.Emit(");\n}\n")
}

// transform InsnDescription to syntax example, e.g. "addi.d d, j, sk12"
func insnSyntaxDescForInsn(d *common.InsnDescription) string {
	if len(d.Format.Args) == 0 {
		// special-case EMPTY
		return d.Mnemonic
	}

	var sb strings.Builder

	sb.WriteString(d.Mnemonic)
	for i, a := range d.Format.Args {
		if i == 0 {
			sb.WriteRune(' ')
		} else {
			sb.WriteString(", ")
		}

		sb.WriteString(strings.ToLower(a.CanonicalRepr()))
	}

	return sb.String()
}

func emitTCGEmitterForInsn(ectx *common.EmitterCtx, d *common.InsnDescription) {
	opc := insnMnemonicToEnumVariantName(d.Mnemonic)
	opcLower := strings.ToLower(opc)
	argFieldDescs := fieldDescsForArgs(d.Format.Args)

	// docstring line
	ectx.Emit("\n/* Emits the `%s` instruction.  */\n", insnSyntaxDescForInsn(d))

	// function header
	ectx.Emit("static void %s\ntcg_out_%s(TCGContext *s", attribUnused, opcLower)
	for _, fd := range argFieldDescs {
		ectx.Emit(", %s %s", fd.typ, fd.name)
	}
	ectx.Emit(")\n{\n")

	if len(d.Format.Args) == 0 {
		// special-case EMPTY
		ectx.Emit("    tcg_out32(s, %s);\n", opc)
		ectx.Emit("}\n")
		return
	}

	// body and tail
	fmtEncoderFnName := fmtEncoderFnNameForInsnFormat(d.Format)

	ectx.Emit("    tcg_out32(s, %s(%s", fmtEncoderFnName, opc)
	for _, fd := range argFieldDescs {
		ectx.Emit(", %s", fd.name)
	}
	ectx.Emit("));\n")

	ectx.Emit("}\n")
}
