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

// Reader represents a PDF file reader
type Reader struct {
	file    *os.File
	size    int64
	catalog map[string]interface{}
	pages   []map[string]interface{}
}

// Open opens a PDF file for reading
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

// NumPage returns the number of pages
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

// GetPlainText extracts all text from the PDF
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
		return data
	}

	return decompressed
}

// extractTextFromStream extracts text from a content stream
func (r *Reader) extractTextFromStream(data []byte) string {
	var result strings.Builder
	content := string(data)

	// Extract hex strings: <48656C6C6F>
	hexPattern := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*(?:Tj|TJ)`)
	hexMatches := hexPattern.FindAllStringSubmatch(content, -1)
	for _, match := range hexMatches {
		if len(match) > 1 {
			decoded := decodeHexString(match[1])
			if decoded != "" {
				result.WriteString(decoded)
				result.WriteString(" ")
			}
		}
	}

	// Extract literal strings: (text) followed by text operators
	literalPattern := regexp.MustCompile(`\(([^)\\]*(?:\\.[^)\\]*)*)\)\s*(?:Tj|TJ|'|")`)
	literalMatches := literalPattern.FindAllStringSubmatch(content, -1)
	for _, match := range literalMatches {
		if len(match) > 1 {
			decoded := decodeLiteralString(match[1])
			// Only include if it's mostly printable ASCII
			if isPrintableText(decoded) {
				result.WriteString(decoded)
				result.WriteString(" ")
			}
		}
	}

	// Extract array strings: [(text1)(text2)]TJ
	arrayPattern := regexp.MustCompile(`\[((?:[^][]|\([^)]*\))*)\]\s*TJ`)
	arrayMatches := arrayPattern.FindAllStringSubmatch(content, -1)
	for _, match := range arrayMatches {
		if len(match) > 1 {
			arrayContent := match[1]
			stringPattern := regexp.MustCompile(`\(([^)\\]*(?:\\.[^)\\]*)*)\)`)
			strings := stringPattern.FindAllStringSubmatch(arrayContent, -1)
			for _, str := range strings {
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

	return cleanText(result.String())
}

// isPrintableText checks if a string is mostly readable text
func isPrintableText(s string) bool {
	if len(s) == 0 {
		return false
	}

	// Check if string contains mostly ASCII letters, numbers, spaces, and common punctuation
	asciiCount := 0
	totalCount := 0

	for _, r := range s {
		totalCount++
		// Count letters, numbers, spaces, and common punctuation
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == ' ' || r == '.' ||
			r == ',' || r == '!' || r == '?' || r == '\'' ||
			r == '"' || r == ':' || r == ';' || r == '-' ||
			r == '\n' || r == '\r' || r == '\t' || r == '(' || r == ')' {
			asciiCount++
		}
	}

	// Require at least 90% standard ASCII characters
	// and the string must contain at least one letter
	hasLetter := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
			break
		}
	}

	return hasLetter && float64(asciiCount)/float64(totalCount) >= 0.9
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
	// First, add spaces between likely word boundaries (capital letters, numbers)
	var spaced strings.Builder
	for i, r := range s {
		if i > 0 {
			prev := rune(s[i-1])
			// Add space before capital letter if previous was lowercase
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				spaced.WriteString(" ")
			}
			// Add space between letter and number or number and letter
			if (unicode.IsLetter(r) && unicode.IsDigit(prev)) ||
				(unicode.IsDigit(r) && unicode.IsLetter(prev)) {
				spaced.WriteString(" ")
			}
		}
		spaced.WriteRune(r)
	}

	// Split into lines and filter
	lines := strings.Split(spaced.String(), "\n")
	var cleaned []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines or lines with very few characters
		if len(line) < 10 {
			continue
		}

		// Count readable words (sequences of 3+ letters)
		wordPattern := regexp.MustCompile(`[a-zA-Z]{3,}`)
		words := wordPattern.FindAllString(line, -1)

		// Skip lines without at least 2 real words
		if len(words) < 2 {
			continue
		}

		// Skip lines where readable words make up less than 60% of the content
		totalWordChars := 0
		for _, w := range words {
			totalWordChars += len(w)
		}
		if float64(totalWordChars)/float64(len(line)) < 0.6 {
			continue
		}

		// Additional check: skip lines with too many single characters or numbers
		singleChars := regexp.MustCompile(`\b[A-Z0-9]\b`).FindAllString(line, -1)
		if len(singleChars) > len(words)/2 {
			continue
		}

		cleaned = append(cleaned, line)
	}

	// Join with newlines and clean up excessive spacing
	result := strings.Join(cleaned, "\n")
	result = regexp.MustCompile(`\s+`).ReplaceAllString(result, " ")
	result = strings.TrimSpace(result)

	// Add back newlines for better readability (every ~80 chars or at sentence end)
	words := strings.Fields(result)
	var formatted strings.Builder
	lineLen := 0

	for i, word := range words {
		if lineLen > 0 && lineLen+len(word)+1 > 80 {
			formatted.WriteString("\n")
			lineLen = 0
		}

		if lineLen > 0 {
			formatted.WriteString(" ")
			lineLen++
		}

		formatted.WriteString(word)
		lineLen += len(word)

		// Add newline after sentence-ending punctuation
		if i < len(words)-1 && len(word) > 0 {
			lastChar := word[len(word)-1]
			if lastChar == '.' || lastChar == '!' || lastChar == '?' {
				formatted.WriteString("\n")
				lineLen = 0
			}
		}
	}

	return formatted.String()
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
