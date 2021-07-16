package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strings"

	"github.com/loongson-community/loongarch-opcodes/scripts/go/common"
)

func main() {
	inputs := os.Args[1:]

	descs, err := readInsnDescs(inputs)
	if err != nil {
		panic(err)
	}

	formats := gatherFormats(descs)

	sort.Slice(descs, func(i int, j int) bool {
		return descs[i].Word < descs[j].Word
	})

	sort.Slice(formats, func(i int, j int) bool {
		return formats[i].CanonicalRepr() < formats[j].CanonicalRepr()
	})

	var ectx emitterCtx

	ectx.emit("package loong\n\n")
	ectx.emit("import \"cmd/internal/obj\"\n\n")

	emitInsnFormatTypes(&ectx, formats)

	for _, f := range formats {
		emitValidatorForFormat(&ectx, f)
		emitEncoderForFormat(&ectx, f)
	}

	emitInsnEncodings(&ectx, descs)

	result := ectx.finalize()
	os.Stdout.Write(result)
}

////////////////////////////////////////////////////////////////////////////

func readInsnDescs(paths []string) ([]*common.InsnDescription, error) {
	var result []*common.InsnDescription
	for _, path := range paths {
		descs, err := common.ReadInsnDescriptionFile(path)
		if err != nil {
			return nil, err
		}
		result = append(result, descs...)
	}
	return result, nil
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

////////////////////////////////////////////////////////////////////////////

type emitterCtx struct {
	buf bytes.Buffer
}

func (c *emitterCtx) emit(format string, a ...interface{}) {
	fmt.Fprintf(&c.buf, format, a...)
}

func (c *emitterCtx) finalize() []byte {
	result, err := format.Source(c.buf.Bytes())
	if err != nil {
		panic(err)
	}

	return result
}

////////////////////////////////////////////////////////////////////////////

func emitInsnFormatTypes(ectx *emitterCtx, fmts []*common.InsnFormat) {
	ectx.emit("type insnFormat int\n\nconst (\n")
	ectx.emit("\tinsnFormatUnknown insnEncoding = iota\n")

	for _, f := range fmts {
		ectx.emit("\tinsnFormat%s\n", f.CanonicalRepr())
	}

	ectx.emit(")\n\n")
}

func goOpcodeNameForInsn(mnemonic string) string {
	// e.g. slli.w => ASLLIW
	tmp := strings.ReplaceAll(mnemonic, ".", "")
	tmp = strings.ReplaceAll(tmp, "_", "")
	tmp = strings.ToUpper(tmp)
	return "A" + tmp
}

func emitInsnEncodings(ectx *emitterCtx, descs []*common.InsnDescription) {
	ectx.emit("type encoding struct {\n")
	ectx.emit("\tbits uint32\n")
	ectx.emit("\tfmt  insnFormat\n")
	ectx.emit("}\n\n")
	ectx.emit("var encodings = [ALAST & obj.AMask]encoding{\n")

	for _, d := range descs {
		goOpcodeName := goOpcodeNameForInsn(d.Mnemonic)
		formatName := "insnFormat" + d.Format.CanonicalRepr()

		ectx.emit(
			"\t%s & obj.AMask: {bits: 0x%08x, fmt: %s},\n",
			goOpcodeName,
			d.Word,
			formatName,
		)
	}

	ectx.emit("}\n")
}

func emitValidatorForFormat(ectx *emitterCtx, f *common.InsnFormat) {
	formatName := f.CanonicalRepr()
	funcName := "validate" + formatName

	argNames := make([]string, len(f.Args))
	for i, a := range f.Args {
		argNames[i] = strings.ToLower(a.CanonicalRepr())
	}

	ectx.emit("func %s(", funcName)
	for i, p := range argNames {
		var sep string
		if i > 0 {
			sep = ", "
		}
		ectx.emit("%s%s uint32", sep, p)
	}
	ectx.emit(") error {\n")

	// things to emit:
	//
	// for every arg X:
	//     if err := want<arg type>("argX", argX); err != nil {
	//         return err
	//     }
	for argIdx, a := range f.Args {
		argParamName := argNames[argIdx]

		ectx.emit("\tif err := ")

		switch a.Kind {
		case common.ArgKindIntReg:
			ectx.emit("wantIntReg(%s)", argParamName)

		case common.ArgKindFPReg:
			ectx.emit("wantFPReg(%s)", argParamName)

		case common.ArgKindFCCReg:
			ectx.emit("wantFCCReg(%s)", argParamName)

		case common.ArgKindSignedImm,
			common.ArgKindUnsignedImm:
			// want[Un]signedImm(argX, width)
			var wantFuncName string
			if a.Kind == common.ArgKindSignedImm {
				wantFuncName = "wantSignedImm"
			} else {
				wantFuncName = "wantUnsignedImm"
			}

			ectx.emit("%s(%s, %d)", wantFuncName, argParamName, a.TotalWidth())
		}

		ectx.emit("; err != nil {\n\t\treturn err\n\t}\n")
	}

	ectx.emit("\treturn nil\n}\n\n")
}

func emitEncoderForFormat(ectx *emitterCtx, f *common.InsnFormat) {
	formatName := f.CanonicalRepr()
	funcName := "encode" + formatName

	argNames := make([]string, len(f.Args))
	for i, a := range f.Args {
		argNames[i] = strings.ToLower(a.CanonicalRepr())
	}

	// func encodeXXX(bits uint32, params...) uint32 {
	ectx.emit("func %s(bits uint32", funcName)
	for _, p := range argNames {
		ectx.emit(", %s uint32", p)
	}
	ectx.emit(") uint32 {\n")

	// things to emit:
	//
	// for every arg X:
	//     if only one slot:
	//         bits |= argX << slot offset
	//
	//     else for every slot in arg:
	//         slot value = (extract from argX)
	//         bits |= slot value << slot offset
	for argIdx, a := range f.Args {
		argParamName := argNames[argIdx]

		if len(a.Slots) == 1 {
			ectx.emit("\tbits |= %s", argParamName)
			offset := int(a.Slots[0].Offset)
			if offset != 0 {
				ectx.emit(" << %d", offset)
			}
			ectx.emit("\n")
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
			// emit bits |= (d5 expr above)
			//
			// slot k16: remainingBits = 0
			// thus k16 = (sd5k16 >> 0) & 0b1111111111111111
			//          = sd5k16 & 0b1111111111111111
			// emit bits |= (k16 expr above)
			remainingBits := int(a.TotalWidth())
			for _, s := range a.Slots {
				remainingBits -= int(s.Width)
				mask := int((1 << s.Width) - 1)

				ectx.emit("\tbits |= %s", argParamName)

				if remainingBits > 0 {
					ectx.emit(" >> %d", remainingBits)
				}

				ectx.emit(" & %#x\n", mask)
			}
		}
	}

	ectx.emit("\treturn bits\n}\n\n")
}
