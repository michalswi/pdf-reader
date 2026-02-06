<div align="center">

# PDF Reader

[![stars](https://img.shields.io/github/stars/michalswi/pdf-reader?style=for-the-badge&color=353535)](https://github.com/michalswi/pdf-reader)
[![forks](https://img.shields.io/github/forks/michalswi/pdf-reader?style=for-the-badge&color=353535)](https://github.com/michalswi/pdf-reader/fork)
[![releases](https://img.shields.io/github/v/release/michalswi/pdf-reader?style=for-the-badge&color=353535)](https://github.com/michalswi/pdf-reader/releases)

go-cli tool to extract text from **PDF** files and save it to a **text** file

</div>


## Features

- Extract plain text from PDF files
- Output to stdout or save to a file

## \# installation

```bash
make build

or

make build-mac      # [rename file after creation]
make build-linux    # [rename file after creation] 
```

## \# options

- `-o <file>` - Output file path (default: stdout)
- `-v` - Verbose output (prints progress to stderr)

## \# usage

### Basic usage (output to stdout):
```bash
./pdf-reader document.pdf
```

### Save to a file:
```bash
./pdf-reader -o output.txt document.pdf
```

### Verbose mode:
```bash
./pdf-reader -v document.pdf
```

## Library Usage

```go
import "github.com/michalswi/pdf-reader/pdf"

// Open PDF
f, reader, err := pdf.Open("document.pdf")
if err != nil {
    panic(err)
}
defer f.Close()

// Extract text
textReader, _ := reader.GetPlainText()
text, _ := io.ReadAll(textReader)
fmt.Println(string(text))

// Get page count
pages := reader.NumPage()
```
