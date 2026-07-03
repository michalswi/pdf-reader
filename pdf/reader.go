package pdf

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Reader represents a PDF file reader.
// Provides methods to extract text and get page information.
type Reader struct {
	file      *os.File
	size      int64
	data      []byte
	xref      map[int]int64                // objNum -> byte offset of the "N G obj" line
	fontCmaps map[string]map[uint32]string // font resource name -> glyph code -> Unicode string
}

// Open opens a PDF file and returns a Reader.
// Returns the file handle (must be closed by caller), a Reader, and any error.
func Open(filename string) (*os.File, *Reader, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, err
	}

	fileInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}

	reader := &Reader{
		file:      file,
		size:      fileInfo.Size(),
		xref:      make(map[int]int64),
		fontCmaps: make(map[string]map[uint32]string),
	}

	if err := reader.parse(); err != nil {
		file.Close()
		return nil, nil, err
	}

	return file, reader, nil
}

// parse reads and initializes the PDF structure.
func (r *Reader) parse() error {
	r.data = make([]byte, r.size)
	if _, err := r.file.ReadAt(r.data, 0); err != nil {
		return err
	}

	if !bytes.HasPrefix(r.data, []byte("%PDF-")) {
		return fmt.Errorf("not a valid PDF file")
	}

	r.scanObjects()
	r.loadFontCmaps()
	return nil
}

// ── Object index ─────────────────────────────────────────────────────────────

// scanObjects builds the xref index by scanning for "N G obj" patterns.
// This works for both traditional xref tables and cross-reference streams
// (PDF 1.5+) without requiring a full xref parser.
func (r *Reader) scanObjects() {
	re := regexp.MustCompile(`(?m)^(\d+)\s+\d+\s+obj\b`)
	for _, m := range re.FindAllSubmatchIndex(r.data, -1) {
		objNum, _ := strconv.Atoi(string(r.data[m[2]:m[3]]))
		if _, exists := r.xref[objNum]; !exists {
			r.xref[objNum] = int64(m[0])
		}
	}
}

// readObjectData returns the raw bytes of object N's body (between "obj" and "endobj").
func (r *Reader) readObjectData(objNum int) []byte {
	offset, ok := r.xref[objNum]
	if !ok {
		return nil
	}
	start := int(offset)
	scanEnd := start + 100
	if scanEnd > len(r.data) {
		scanEnd = len(r.data)
	}
	objIdx := bytes.Index(r.data[start:scanEnd], []byte("obj"))
	if objIdx == -1 {
		return nil
	}
	bodyStart := start + objIdx + 3 // right after "obj"

	endIdx := bytes.Index(r.data[bodyStart:], []byte("endobj"))
	if endIdx == -1 {
		return nil
	}
	return r.data[bodyStart : bodyStart+endIdx]
}

// readStreamData reads and decompresses the stream of indirect object N.
func (r *Reader) readStreamData(objNum int) []byte {
	body := r.readObjectData(objNum)
	if body == nil {
		return nil
	}
	s := string(body)
	streamIdx := strings.Index(s, "stream")
	if streamIdx == -1 {
		return nil
	}
	after := streamIdx + len("stream")
	if after < len(s) && s[after] == '\r' {
		after++
	}
	if after < len(s) && s[after] == '\n' {
		after++
	}
	endIdx := strings.Index(s[after:], "endstream")
	if endIdx == -1 {
		return nil
	}
	raw := body[after : after+endIdx]
	if strings.Contains(s[:streamIdx], "FlateDecode") {
		if dec := tryDecompress(raw); dec != nil {
			return dec
		}
	}
	return raw
}

// tryDecompress attempts zlib decompression; returns nil on failure.
func tryDecompress(data []byte) []byte {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	defer zr.Close()
	dec, err := io.ReadAll(zr)
	if err != nil && len(dec) < 20 {
		return nil
	}
	return dec
}

// ── ToUnicode CMap loading ────────────────────────────────────────────────────

// loadFontCmaps builds fontCmaps: font resource name -> ToUnicode CMap.
// It first locates all font objects (by /Type /Font + /ToUnicode), parses their
// CMap streams, then scans /Font << ... >> resource dicts to connect resource
// names (e.g. "F1") to the correct CMap.
func (r *Reader) loadFontCmaps() {
	typeRE := regexp.MustCompile(`/Type\s*/Font\b`)
	tuRE := regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`)

	// Step 1: font object number -> its ToUnicode CMap
	fontObjToCmap := make(map[int]map[uint32]string)
	for objNum := range r.xref {
		body := r.readObjectData(objNum)
		if body == nil || !typeRE.Match(body) {
			continue
		}
		m := tuRE.FindSubmatch(body)
		if m == nil {
			continue
		}
		tuObj, err := strconv.Atoi(string(m[1]))
		if err != nil {
			continue
		}
		streamData := r.readStreamData(tuObj)
		if streamData == nil {
			continue
		}
		cmap := parseCMap(streamData)
		if len(cmap) > 0 {
			fontObjToCmap[objNum] = cmap
		}
	}

	// Step 2: scan the entire file for /Font << ... >> sections and map
	// resource names to their CMaps.
	r.extractFontNames(fontObjToCmap)
}

// extractFontNames scans the whole file for /Font << ... >> dicts and
// populates r.fontCmaps with font resource name -> CMap entries.
func (r *Reader) extractFontNames(fontObjToCmap map[int]map[uint32]string) {
	refRE := regexp.MustCompile(`/(\w+)\s+(\d+)\s+\d+\s+R`)
	fontKey := []byte("/Font")
	pos := 0

	for {
		idx := bytes.Index(r.data[pos:], fontKey)
		if idx == -1 {
			break
		}
		absIdx := pos + idx
		after := r.data[absIdx+5:] // bytes after "/Font"

		// Skip PDF whitespace
		i := 0
		for i < len(after) && isPDFWhitespace(after[i]) {
			i++
		}

		if i+1 < len(after) && after[i] == '<' && after[i+1] == '<' {
			dictContent := extractDictContent(after[i+2:])
			for _, m := range refRE.FindAllStringSubmatch(dictContent, -1) {
				fontObjNum, err := strconv.Atoi(m[2])
				if err != nil {
					continue
				}
				if cmap, ok := fontObjToCmap[fontObjNum]; ok {
					r.fontCmaps[m[1]] = cmap
				}
			}
		}

		pos = absIdx + 5
	}
}

// isPDFWhitespace reports whether b is a PDF whitespace character.
func isPDFWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' || b == 0
}

// extractDictContent returns the content of a << ... >> dict (depth-aware),
// given the bytes immediately after the opening <<.
func extractDictContent(data []byte) string {
	depth := 1
	i := 0
	for i < len(data) && depth > 0 {
		if i+1 < len(data) && data[i] == '<' && data[i+1] == '<' {
			depth++
			i += 2
		} else if i+1 < len(data) && data[i] == '>' && data[i+1] == '>' {
			depth--
			i += 2
		} else {
			i++
		}
	}
	return string(data[:i])
}

// ── CMap parser ───────────────────────────────────────────────────────────────

// parseCMap parses a PDF ToUnicode CMap stream into a glyph code -> Unicode string map.
func parseCMap(data []byte) map[uint32]string {
	result := make(map[uint32]string)
	content := string(data)

	bfcharRE := regexp.MustCompile(`(?s)beginbfchar(.*?)endbfchar`)
	for _, m := range bfcharRE.FindAllStringSubmatch(content, -1) {
		parseBfChar(m[1], result)
	}

	bfrangeRE := regexp.MustCompile(`(?s)beginbfrange(.*?)endbfrange`)
	for _, m := range bfrangeRE.FindAllStringSubmatch(content, -1) {
		parseBfRange(m[1], result)
	}

	return result
}

// parseBfChar parses beginbfchar entries of the form: <srcCode> <dstCode>
func parseBfChar(content string, result map[uint32]string) {
	re := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s+<([0-9A-Fa-f]+)>`)
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		srcBytes, err := hex.DecodeString(m[1])
		if err != nil || len(srcBytes) == 0 {
			continue
		}
		dstBytes, err := hex.DecodeString(m[2])
		if err != nil || len(dstBytes) == 0 {
			continue
		}
		result[bytesToCode(srcBytes)] = utf16BEToString(dstBytes)
	}
}

// parseBfRange parses beginbfrange entries.
// Supports both single-destination (<lo> <hi> <dst>) and array forms (<lo> <hi> [...]).
func parseBfRange(content string, result map[uint32]string) {
	// Array form first: <lo> <hi> [<d0> <d1> ...]
	arrayRE := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s+<([0-9A-Fa-f]+)>\s+\[([^\]]*)\]`)
	for _, m := range arrayRE.FindAllStringSubmatch(content, -1) {
		loBytes, _ := hex.DecodeString(m[1])
		hiBytes, _ := hex.DecodeString(m[2])
		if len(loBytes) == 0 || len(hiBytes) == 0 {
			continue
		}
		lo := bytesToCode(loBytes)
		hi := bytesToCode(hiBytes)

		hexRE := regexp.MustCompile(`<([0-9A-Fa-f]+)>`)
		for i, hm := range hexRE.FindAllStringSubmatch(m[3], -1) {
			code := lo + uint32(i)
			if code > hi {
				break
			}
			dstBytes, err := hex.DecodeString(hm[1])
			if err != nil {
				continue
			}
			result[code] = utf16BEToString(dstBytes)
		}
	}

	// Single destination: <lo> <hi> <dst>
	singleRE := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s+<([0-9A-Fa-f]+)>\s+<([0-9A-Fa-f]+)>`)
	for _, m := range singleRE.FindAllStringSubmatch(content, -1) {
		loBytes, _ := hex.DecodeString(m[1])
		hiBytes, _ := hex.DecodeString(m[2])
		dstBytes, err := hex.DecodeString(m[3])
		if err != nil || len(loBytes) == 0 || len(hiBytes) == 0 || len(dstBytes) == 0 {
			continue
		}
		lo := bytesToCode(loBytes)
		hi := bytesToCode(hiBytes)
		dstCode := bytesToCode(dstBytes)
		for code := lo; code <= hi; code++ {
			result[code] = string(rune(dstCode + (code - lo)))
		}
	}
}

// bytesToCode converts 1-4 bytes (big-endian) to a uint32 glyph code.
func bytesToCode(b []byte) uint32 {
	var code uint32
	for _, by := range b {
		code = code<<8 | uint32(by)
	}
	return code
}

// utf16BEToString converts UTF-16 Big Endian bytes to a Go string.
// A 1-byte value is treated as a direct Unicode code point.
func utf16BEToString(data []byte) string {
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		data = data[2:]
	}
	if len(data) == 1 {
		return string(rune(data[0]))
	}
	var sb strings.Builder
	for i := 0; i+1 < len(data); i += 2 {
		r := rune(uint16(data[i])<<8 | uint16(data[i+1]))
		if r != 0 {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// applyToUnicode maps raw glyph bytes through a ToUnicode CMap.
// Tries 2-byte codes first (most common in modern PDFs), then 1-byte.
func applyToUnicode(data []byte, cmap map[uint32]string) string {
	var sb strings.Builder
	i := 0
	for i < len(data) {
		if i+1 < len(data) {
			if s, ok := cmap[uint32(data[i])<<8|uint32(data[i+1])]; ok {
				sb.WriteString(s)
				i += 2
				continue
			}
		}
		if s, ok := cmap[uint32(data[i])]; ok {
			sb.WriteString(s)
		}
		i++
	}
	return sb.String()
}

// ── Public API ────────────────────────────────────────────────────────────────

// NumPage returns the number of pages in the PDF.
func (r *Reader) NumPage() int {
	count := bytes.Count(r.data, []byte("/Type /Page"))
	if count == 0 {
		count = 1
	}
	return count
}

// Page returns a page by number (1-indexed).
func (r *Reader) Page(num int) Page {
	return Page{reader: r, number: num}
}

// GetPlainText extracts all text from the PDF and returns it as an io.Reader.
func (r *Reader) GetPlainText() (io.Reader, error) {
	var result bytes.Buffer
	result.WriteString(r.extractTextFromData(r.data))
	return &result, nil
}

// ── Content stream processing ─────────────────────────────────────────────────

// extractTextFromData scans all streams in the PDF data and extracts text content.
func (r *Reader) extractTextFromData(data []byte) string {
	var result strings.Builder

	streamStart := []byte("stream")
	streamEnd := []byte("endstream")
	nonContentRE := regexp.MustCompile(`/Subtype\s*/Image|/Type\s*/Font|/Subtype\s*/CIDFontType|BitsPerComponent`)

	pos := 0
	for {
		idx := bytes.Index(data[pos:], streamStart)
		if idx == -1 {
			break
		}
		streamStartPos := pos + idx + len(streamStart)
		for streamStartPos < len(data) && (data[streamStartPos] == '\r' || data[streamStartPos] == '\n') {
			streamStartPos++
		}
		endIdx := bytes.Index(data[streamStartPos:], streamEnd)
		if endIdx == -1 {
			break
		}
		streamData := data[streamStartPos : streamStartPos+endIdx]

		// Skip non-text streams (images, embedded font programs, etc.)
		lookbackStart := pos + idx - 200
		if lookbackStart < 0 {
			lookbackStart = 0
		}
		preceding := string(data[lookbackStart : pos+idx])
		if nonContentRE.MatchString(preceding) {
			pos = streamStartPos + endIdx + len(streamEnd)
			continue
		}

		decompressed := tryDecompress(streamData)
		if decompressed == nil {
			decompressed = streamData
		}

		text := r.extractTextFromStream(decompressed)
		if text != "" {
			result.WriteString(text)
			result.WriteString("\n")
		}

		pos = streamStartPos + endIdx + len(streamEnd)
	}

	return result.String()
}

// matrix represents a PDF 2D transformation matrix [a b c d e f] in row-vector
// convention: x' = a*x + c*y + e,  y' = b*x + d*y + f.
type matrix [6]float64

// matMul concatenates two PDF matrices: result = m1 × m2.
func matMul(m1, m2 matrix) matrix {
	return matrix{
		m1[0]*m2[0] + m1[1]*m2[2],
		m1[0]*m2[1] + m1[1]*m2[3],
		m1[2]*m2[0] + m1[3]*m2[2],
		m1[2]*m2[1] + m1[3]*m2[3],
		m1[4]*m2[0] + m1[5]*m2[2] + m2[4],
		m1[4]*m2[1] + m1[5]*m2[3] + m2[5],
	}
}

// applyMatrix transforms (x, y) through m into device coordinates.
func applyMatrix(m matrix, x, y float64) (float64, float64) {
	return m[0]*x + m[2]*y + m[4], m[1]*x + m[3]*y + m[5]
}

// textRun is a positioned piece of decoded text from a content stream.
type textRun struct {
	x, y, sz float64
	text     string
}

// extractTextFromStream processes a PDF content stream token by token,
// tracking the CTM (via q/Q/cm) and text state (font, position) so that each
// glyph is decoded with the correct ToUnicode CMap and positioned in device
// space for correct reading-order assembly.
func (r *Reader) extractTextFromStream(data []byte) string {
	tokens := tokenizePDF(string(data))
	n := len(tokens)

	var runs []textRun

	// Graphics state: current transformation matrix and save/restore stack.
	ctm := matrix{1, 0, 0, 1, 0, 0}
	var ctmStack []matrix

	// Text state — persists across BT/ET blocks within the same stream.
	// tm tracks the full 6-component text matrix so that Td is applied
	// correctly through the matrix orientation (important when d ≠ 1).
	inText := false
	var activeCmap map[uint32]string
	var fontSize float64 = 12
	var tm = matrix{1, 0, 0, 1, 0, 0} // text matrix

	addRun := func(text string) {
		if text == "" {
			return
		}
		// Text origin in device space = CTM applied to (tm[4], tm[5]).
		px, py := applyMatrix(ctm, tm[4], tm[5])
		effSz := fontSize * math.Abs(ctm[0])
		if effSz == 0 {
			effSz = fontSize
		}
		runs = append(runs, textRun{x: px, y: py, sz: effSz, text: text})
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]
		switch tok {
		case "q":
			ctmStack = append(ctmStack, ctm)
		case "Q":
			if len(ctmStack) > 0 {
				ctm = ctmStack[len(ctmStack)-1]
				ctmStack = ctmStack[:len(ctmStack)-1]
			}
		case "cm":
			// a b c d e f cm — concatenate matrix with current CTM (pre-multiply).
			if i >= 6 {
				var m matrix
				for k := 0; k < 6; k++ {
					m[k], _ = strconv.ParseFloat(tokens[i-6+k], 64)
				}
				ctm = matMul(m, ctm)
			}
		case "BT":
			inText = true
			tm = matrix{1, 0, 0, 1, 0, 0} // PDF spec: BT resets Tm and Tlm to identity
		case "ET":
			inText = false
		case "Tf":
			// /FontName size Tf
			if inText && i >= 2 {
				if sz, err := strconv.ParseFloat(tokens[i-1], 64); err == nil && sz > 0 {
					fontSize = sz
				}
				activeCmap = r.fontCmaps[strings.TrimPrefix(tokens[i-2], "/")]
			}
		case "Tm":
			// a b c d e f Tm — set text matrix (all 6 components).
			if inText && i >= 6 {
				for k := 0; k < 6; k++ {
					tm[k], _ = strconv.ParseFloat(tokens[i-6+k], 64)
				}
			}
		case "Td", "TD":
			// tx ty Td — pre-multiply Tlm by Translation(tx,ty).
			// New e = tx*tm[0] + ty*tm[2] + tm[4]
			// New f = tx*tm[1] + ty*tm[3] + tm[5]
			if inText && i >= 2 {
				tx, _ := strconv.ParseFloat(tokens[i-2], 64)
				ty, _ := strconv.ParseFloat(tokens[i-1], 64)
				e := tx*tm[0] + ty*tm[2] + tm[4]
				f := tx*tm[1] + ty*tm[3] + tm[5]
				tm[4], tm[5] = e, f
			}
		case "T*":
			// Equivalent to 0 -leading Td (approximate leading as fontSize).
			if inText {
				ty := -fontSize
				tm[4] = ty*tm[2] + tm[4]
				tm[5] = ty*tm[3] + tm[5]
			}
		case "Tj":
			// string Tj — show text string.
			if inText && i >= 1 {
				addRun(r.decodeToken(tokens[i-1], activeCmap))
			}
		case "TJ":
			// array TJ — show text with individual glyph positioning.
			if inText && i >= 1 {
				arrTok := tokens[i-1]
				if strings.HasPrefix(arrTok, "[") {
					inner := arrTok[1 : len(arrTok)-1]
					addRun(r.processTJArray(inner, activeCmap))
				}
			}
		case "'":
			// string ' — move to next line and show text.
			if inText {
				ty := -fontSize
				tm[4] = ty*tm[2] + tm[4]
				tm[5] = ty*tm[3] + tm[5]
				if i >= 1 {
					addRun(r.decodeToken(tokens[i-1], activeCmap))
				}
			}
		}
	}

	return assembleRuns(runs)
}

// tokenizePDF splits a PDF content stream into individual tokens:
// literal strings "(…)", hex strings "<…>", arrays "[…]", dicts "<<…>>",
// and bare tokens (names, numbers, operators).
func tokenizePDF(content string) []string {
	var tokens []string
	i, n := 0, len(content)

	for i < n {
		// Skip PDF whitespace
		for i < n && isPDFWhitespace(content[i]) {
			i++
		}
		if i >= n {
			break
		}

		switch content[i] {
		case '%':
			// Comment: skip to end of line
			for i < n && content[i] != '\n' {
				i++
			}

		case '(':
			// Literal string "(…)"
			start := i
			i++
			depth := 1
			for i < n && depth > 0 {
				switch content[i] {
				case '\\':
					i++ // skip backslash
					if i < n {
						i++ // skip escaped character
					}
				case '(':
					depth++
					i++
				case ')':
					depth--
					i++
				default:
					i++
				}
			}
			tokens = append(tokens, content[start:i])

		case '<':
			if i+1 < n && content[i+1] == '<' {
				// Dictionary "<<…>>"
				start := i
				i += 2
				depth := 1
				for i < n && depth > 0 {
					if i+1 < n && content[i] == '<' && content[i+1] == '<' {
						depth++
						i += 2
					} else if i+1 < n && content[i] == '>' && content[i+1] == '>' {
						depth--
						i += 2
					} else {
						i++
					}
				}
				tokens = append(tokens, content[start:i])
			} else {
				// Hex string "<…>"
				start := i
				i++
				for i < n && content[i] != '>' {
					i++
				}
				if i < n {
					i++ // skip '>'
				}
				tokens = append(tokens, content[start:i])
			}

		case '[':
			// Array "[…]" — handles nested literal strings and hex strings
			start := i
			i++
			depth := 1
			for i < n && depth > 0 {
				switch content[i] {
				case '[':
					depth++
					i++
				case ']':
					depth--
					i++
				case '(':
					// Literal string inside array
					i++
					d := 1
					for i < n && d > 0 {
						if content[i] == '\\' {
							i++ // skip backslash
							if i < n {
								i++ // skip escaped character
							}
						} else if content[i] == '(' {
							d++
							i++
						} else if content[i] == ')' {
							d--
							i++
						} else {
							i++
						}
					}
				case '<':
					// Hex string or dict inside array
					i++
					if i < n && content[i] == '<' {
						i++
						dd := 1
						for i < n && dd > 0 {
							if i+1 < n && content[i] == '<' && content[i+1] == '<' {
								dd++
								i += 2
							} else if i+1 < n && content[i] == '>' && content[i+1] == '>' {
								dd--
								i += 2
							} else {
								i++
							}
						}
					} else {
						for i < n && content[i] != '>' {
							i++
						}
						if i < n {
							i++ // skip '>'
						}
					}
				default:
					i++
				}
			}
			tokens = append(tokens, content[start:i])

		default:
			// Name, number, or operator
			start := i
			for i < n && !isPDFWhitespace(content[i]) &&
				content[i] != '(' && content[i] != '<' && content[i] != '[' &&
				content[i] != '%' {
				i++
			}
			if i > start {
				tokens = append(tokens, content[start:i])
			}
		}
	}
	return tokens
}

// decodeToken decodes a PDF string token (literal or hex) using the given CMap.
func (r *Reader) decodeToken(tok string, cmap map[uint32]string) string {
	tok = strings.TrimSpace(tok)
	switch {
	case strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">"):
		return r.decodeHexString(tok[1:len(tok)-1], cmap)
	case strings.HasPrefix(tok, "(") && strings.HasSuffix(tok, ")"):
		s := decodeLiteralString(tok[1 : len(tok)-1])
		if isPrintableText(s) {
			return s
		}
	}
	return ""
}

// assembleRuns builds a readable string from positioned text runs.
// Runs are sorted by y then x (top-to-bottom, left-to-right).
// A newline is inserted between runs on different lines; a space is inserted
// where the horizontal gap is larger than a typical character advance.
func assembleRuns(runs []textRun) string {

	if len(runs) == 0 {
		return ""
	}

	// Sort: primary by y descending (high device y = top of page), secondary by x ascending.
	sort.Slice(runs, func(i, j int) bool {
		if math.Abs(runs[i].y-runs[j].y) > 2 {
			return runs[i].y > runs[j].y
		}
		return runs[i].x < runs[j].x
	})

	var sb strings.Builder
	prev := runs[0]
	// Estimate of run width: 0.7 × font-size × number of runes.
	// Most Latin glyphs advance 60-80 % of the em; 0.7 is a reasonable midpoint
	// that prevents false spaces between consecutive characters while still
	// letting genuinely large x-gaps (> 0.25 × fontSize) be treated as spaces.
	prevEndX := prev.x + float64(len([]rune(prev.text)))*prev.sz*0.7
	sb.WriteString(prev.text)

	for _, run := range runs[1:] {
		dy := math.Abs(run.y - prev.y)
		dx := run.x - prevEndX

		if dy > prev.sz*0.4 {
			// Significant vertical shift → new line.
			sb.WriteByte('\n')
		} else if dx > prev.sz*0.25 {
			// Horizontal gap larger than ~25 % of font size → word space.
			// Space glyphs decoded from the CMap already appear as ' ' in
			// run.text, so this only fires for implicit gaps.
			sb.WriteByte(' ')
		}

		sb.WriteString(run.text)
		prev = run
		prevEndX = run.x + float64(len([]rune(run.text)))*run.sz*0.7
	}

	return cleanText(sb.String())
}

// decodeHexString decodes a PDF hex string, applying the ToUnicode CMap when available.
func (r *Reader) decodeHexString(hexStr string, cmap map[uint32]string) string {
	if len(hexStr)%2 != 0 {
		hexStr += "0"
	}
	decoded, err := hex.DecodeString(hexStr)
	if err != nil {
		return ""
	}
	if cmap != nil {
		return applyToUnicode(decoded, cmap)
	}
	// No CMap: try UTF-16 BE, then fall back to ASCII/Latin-1.
	if len(decoded) >= 2 && decoded[0] == 0xFE && decoded[1] == 0xFF {
		return decodeUTF16BE(decoded[2:])
	}
	return filterPrintable(string(decoded))
}

// processTJArray extracts text from a TJ array, applying the ToUnicode CMap when available.
// It inserts a space for kerning adjustments < -100 text units (inter-word gap convention).
func (r *Reader) processTJArray(content string, cmap map[uint32]string) string {
	var sb strings.Builder
	i := 0
	n := len(content)
	for i < n {
		ch := content[i]
		switch {
		case ch == '(':
			end, decoded := scanLiteralString(content, i)
			if isPrintableText(decoded) {
				sb.WriteString(decoded)
			}
			i = end
		case ch == '<':
			closeIdx := strings.Index(content[i+1:], ">")
			if closeIdx == -1 {
				i++
				continue
			}
			hexStr := content[i+1 : i+1+closeIdx]
			decoded := r.decodeHexString(hexStr, cmap)
			if decoded != "" {
				sb.WriteString(decoded)
			}
			i = i + 1 + closeIdx + 1
		case ch == '-' || (ch >= '0' && ch <= '9'):
			end := i + 1
			for end < n && (content[end] >= '0' && content[end] <= '9' || content[end] == '.') {
				end++
			}
			if val, err := strconv.ParseFloat(content[i:end], 64); err == nil && val < -100 {
				sb.WriteString(" ")
			}
			i = end
		default:
			i++
		}
	}
	return sb.String()
}

// ── String helpers ────────────────────────────────────────────────────────────

// scanLiteralString scans a PDF literal string starting at '(' (handling nested
// parens and backslash escapes) and returns (position after closing ')', decoded text).
func scanLiteralString(s string, pos int) (int, string) {
	if pos >= len(s) || s[pos] != '(' {
		return pos + 1, ""
	}
	i := pos + 1
	depth := 1
	var raw strings.Builder
	for i < len(s) && depth > 0 {
		ch := s[i]
		if ch == '\\' && i+1 < len(s) {
			raw.WriteByte(ch)
			raw.WriteByte(s[i+1])
			i += 2
		} else if ch == '(' {
			depth++
			raw.WriteByte(ch)
			i++
		} else if ch == ')' {
			depth--
			if depth > 0 {
				raw.WriteByte(ch)
			}
			i++
		} else {
			raw.WriteByte(ch)
			i++
		}
	}
	return i, decodeLiteralString(raw.String())
}

// isPrintableText checks if a string is mostly readable text.
// Accepts Unicode letters, digits, punctuation, and whitespace so that
// non-ASCII scripts (e.g. Polish, accented Latin) are not incorrectly rejected.
func isPrintableText(s string) bool {
	if len(s) == 0 {
		return false
	}
	printable := 0
	total := 0
	for _, r := range s {
		total++
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSpace(r) {
			printable++
		}
	}
	return total > 0 && float64(printable)/float64(total) >= 0.6
}

// decodeLiteralString decodes a PDF literal string with escape sequences.
func decodeLiteralString(s string) string {
	var result strings.Builder
	escape := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if escape {
			switch ch {
			case 'n':
				result.WriteByte('\n')
			case 'r':
				result.WriteByte('\r')
			case 't':
				result.WriteByte('\t')
			case 'b':
				result.WriteByte('\b')
			case 'f':
				result.WriteByte('\f')
			case '(', ')', '\\':
				result.WriteByte(ch)
			case '0', '1', '2', '3', '4', '5', '6', '7':
				// Octal escape sequence
				octal := string(ch)
				j := i + 1
				for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
					octal += string(s[j])
					j++
				}
				if val, err := strconv.ParseInt(octal, 8, 32); err == nil && val < 256 {
					result.WriteByte(byte(val))
					i = j - 1
				}
			default:
				result.WriteByte(ch)
			}
			escape = false
			continue
		}

		if ch == '\\' {
			escape = true
			continue
		}

		result.WriteByte(ch)
	}

	return filterPrintable(result.String())
}

// decodeUTF16BE decodes UTF-16 Big Endian bytes
func decodeUTF16BE(data []byte) string {
	if len(data)%2 != 0 {
		return ""
	}

	var result strings.Builder
	for i := 0; i < len(data); i += 2 {
		r := rune(data[i])<<8 | rune(data[i+1])
		if r != 0 {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// filterPrintable removes non-printable characters except common whitespace
func filterPrintable(s string) string {
	var result strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// cleanText cleans up extracted text
func cleanText(s string) string {
	lines := strings.Split(s, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

// Page represents a PDF page
type Page struct {
	reader *Reader
	number int
}

// GetPlainText extracts plain text from the page
func (p Page) GetPlainText() (string, error) {
	// For simplicity, extract from entire document
	// A proper implementation would extract per-page
	reader, err := p.reader.GetPlainText()
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	buf.ReadFrom(reader)
	return buf.String(), nil
}
