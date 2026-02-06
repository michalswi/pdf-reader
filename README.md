# PDF Reader

go-cli tool to extract text from PDF files and save it to a text file

## Features

- Extract plain text from PDF files
- Output to stdout or save to a file
- Simple and easy to use CLI interface
- Verbose mode for debugging

## \# installation

```bash
make build-mac
make build-linux
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
