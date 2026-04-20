package pdf

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Reader represents a PDF file reader.
// Provides methods to extract text and get page information.
type Reader struct {
	file    *os.File
	size    int64
	catalog map[string]interface{}
	pages   []map[string]interface{}
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
		file:  file,
		size:  fileInfo.Size(),
		pages: make([]map[string]interface{}, 0),
	}

	if err := reader.parse(); err != nil {
		file.Close()
		return nil, nil, err
	}

	return file, reader, nil
}

// parse reads and parses the PDF structure
func (r *Reader) parse() error {
	// Verify PDF header
	header := make([]byte, 8)
	if _, err := r.file.ReadAt(header, 0); err != nil {
		return err
	}

	if !bytes.HasPrefix(header, []byte("%PDF-")) {
		return fmt.Errorf("not a valid PDF file")
	}

	// For simplicity, we'll do a basic text extraction
	// A full PDF parser would need to read xref table, trailer, catalog, etc.
	return nil
}

// NumPage returns the number of pages in the PDF.
func (r *Reader) NumPage() int {
	// Read entire file to count pages
	data := make([]byte, r.size)
	r.file.ReadAt(data, 0)

	// Count /Type /Page occurrences
	count := bytes.Count(data, []byte("/Type /Page"))
	if count == 0 {
		count = 1 // At least one page
	}
	return count
}

// Page returns a page by number (1-indexed)
func (r *Reader) Page(num int) Page {
	return Page{reader: r, number: num}
}

// GetPlainText extracts all text from the PDF and returns it as an io.Reader.
func (r *Reader) GetPlainText() (io.Reader, error) {
	var result bytes.Buffer

	// Read entire file
	data := make([]byte, r.size)
	_, err := r.file.ReadAt(data, 0)
	if err != nil {
		return nil, err
	}

	// Extract text from all streams
	text := r.extractTextFromData(data)
	result.WriteString(text)

	return &result, nil
}

// extractTextFromData extracts text from PDF data
func (r *Reader) extractTextFromData(data []byte) string {
	var result strings.Builder

	// Find all stream objects
	streamStart := []byte("stream")
	streamEnd := []byte("endstream")

	pos := 0
	for {
		// Find next stream
		idx := bytes.Index(data[pos:], streamStart)
		if idx == -1 {
			break
		}

		streamStartPos := pos + idx + len(streamStart)

		// Skip whitespace after "stream"
		for streamStartPos < len(data) && (data[streamStartPos] == '\r' || data[streamStartPos] == '\n') {
			streamStartPos++
		}

		// Find stream end
		endIdx := bytes.Index(data[streamStartPos:], streamEnd)
		if endIdx == -1 {
			break
		}

		streamData := data[streamStartPos : streamStartPos+endIdx]

		// Skip non-text streams: look back at the preceding bytes to find the
		// stream dictionary and skip image data and font programs.
		dictLookbackStart := pos + idx - 200
		if dictLookbackStart < 0 {
			dictLookbackStart = 0
		}
		preceding := string(data[dictLookbackStart : pos+idx])
		nonContentDict := regexp.MustCompile(`/Subtype\s*/Image|/Type\s*/Font|/Subtype\s*/CIDFontType|BitsPerComponent`)
		if nonContentDict.MatchString(preceding) {
			pos = streamStartPos + endIdx + len(streamEnd)
			continue
		}

		// Try to decompress if it's a FlateDecode stream
		decompressed := r.tryDecompress(streamData)

		// Extract text content from stream
		text := r.extractTextFromStream(decompressed)
		if text != "" {
			result.WriteString(text)
			result.WriteString("\n")
		}

		pos = streamStartPos + endIdx + len(streamEnd)
	}

	return result.String()
}

// tryDecompress attempts to decompress stream data
func (r *Reader) tryDecompress(data []byte) []byte {
	// Try zlib decompression
	reader := bytes.NewReader(data)
	zr, err := zlib.NewReader(reader)
	if err != nil {
		// Not compressed or different compression
		return data
	}
	defer zr.Close()

	decompressed, err := io.ReadAll(zr)
	if err != nil {
		if len(decompressed) < 20 {
			return data
		}
		return decompressed
	}

	return decompressed
}

// extractTextFromStream extracts text from a content stream
func (r *Reader) extractTextFromStream(data []byte) string {
	content := string(data)

	// Only process streams that contain PDF text objects (BT...ET blocks).
	// Font programs, ICC profiles, images, and other binary streams never
	// contain valid BT/ET sequences with text operators.
	btBlockPattern := regexp.MustCompile(`(?s)\bBT\b(.*?)\bET\b`)
	btBlocks := btBlockPattern.FindAllStringSubmatch(content, -1)
	if len(btBlocks) == 0 {
		return ""
	}

	// Combined pattern matching all text-showing operators in document order:
	// 1. Hex strings:     <hexdata> Tj|TJ
	// 2. Literal strings: (text) Tj|TJ|'|"
	// 3. Array TJ:        [(strings...)] TJ
	combinedPattern := regexp.MustCompile(
		`<([0-9A-Fa-f]+)>\s*(?:Tj|TJ)` +
			`|\(([^)\\]*(?:\\.[^)\\]*)*)\)\s*(?:Tj|TJ|'|")` +
			`|\[((?:[^][]|\([^)]*\))*)\]\s*TJ`,
	)
	innerStringPattern := regexp.MustCompile(`\(([^)\\]*(?:\\.[^)\\]*)*)\)`)

	var result strings.Builder
	for _, block := range btBlocks {
		if len(block) < 2 {
			continue
		}
		matches := combinedPattern.FindAllStringSubmatch(block[1], -1)
		for _, match := range matches {
			switch {
			case match[1] != "": // hex string
				decoded := decodeHexString(match[1])
				if decoded != "" {
					result.WriteString(decoded)
					result.WriteString(" ")
				}
			case match[2] != "": // literal string
				decoded := decodeLiteralString(match[2])
				if isPrintableText(decoded) {
					result.WriteString(decoded)
					result.WriteString(" ")
				}
			case match[3] != "": // array TJ
				strs := innerStringPattern.FindAllStringSubmatch(match[3], -1)
				for _, str := range strs {
					if len(str) > 1 {
						decoded := decodeLiteralString(str[1])
						if isPrintableText(decoded) {
							result.WriteString(decoded)
						}
					}
				}
				result.WriteString(" ")
			}
		}
		result.WriteString("\n")
	}

	return cleanText(result.String())
}

// isPrintableText checks if a string is mostly readable text
func isPrintableText(s string) bool {
	if len(s) == 0 {
		return false
	}

	asciiCount := 0
	totalCount := 0

	for _, r := range s {
		totalCount++
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == ' ' || r == '.' ||
			r == ',' || r == '!' || r == '?' || r == '\'' ||
			r == '"' || r == ':' || r == ';' || r == '-' ||
			r == '\n' || r == '\r' || r == '\t' || r == '(' || r == ')' {
			asciiCount++
		}
	}

	return float64(asciiCount)/float64(totalCount) >= 0.6
}

// decodeHexString decodes a PDF hex string
func decodeHexString(hexStr string) string {
	// Ensure even length
	if len(hexStr)%2 != 0 {
		hexStr += "0"
	}

	decoded, err := hex.DecodeString(hexStr)
	if err != nil {
		return ""
	}

	// Try to decode as UTF-16 BE (common in PDFs)
	if len(decoded) >= 2 && decoded[0] == 0xFE && decoded[1] == 0xFF {
		return decodeUTF16BE(decoded[2:])
	}

	// Otherwise treat as ASCII/Latin1
	return filterPrintable(string(decoded))
}

// decodeLiteralString decodes a PDF literal string with escape sequences
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
