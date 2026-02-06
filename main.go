package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/michalswi/pdf-reader/pdf"

	"github.com/michalswi/color"
)

var (
	outputFile = flag.String("o", "", "Output file path (default: stdout)")
	verbose    = flag.Bool("v", false, "Verbose output")
)

func main() {
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(1)
	}

	inputPath := flag.Arg(0)

	if *verbose {
		fmt.Fprintln(os.Stderr, color.Format(color.BLUE, fmt.Sprintf("Reading PDF: %s", inputPath)))
	}

	text, err := extractTextFromPDF(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Write output
	var output io.Writer = os.Stdout
	if *outputFile != "" {
		f, err := os.Create(*outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		output = f

		if *verbose {
			fmt.Fprintln(os.Stderr, color.Format(color.BLUE, fmt.Sprintf("Writing output to: %s", *outputFile)))
		}
	}

	_, err = io.WriteString(output, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output: %v\n", err)
		os.Exit(1)
	}

	if *verbose {
		fmt.Fprintln(os.Stderr, color.Format(color.GREEN, "Successfully extracted text from PDF"))
	}
}

func extractTextFromPDF(path string) (string, error) {
	// Open the PDF file
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %w", err)
	}
	defer f.Close()

	// Get plain text from all pages
	textReader, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("failed to extract text: %w", err)
	}

	// Read all text into a buffer
	var buf bytes.Buffer
	_, err = buf.ReadFrom(textReader)
	if err != nil {
		return "", fmt.Errorf("failed to read text: %w", err)
	}

	return buf.String(), nil
}

func usage() {
	fmt.Fprintf(os.Stderr, "PDF to Text Converter\n\n")
	fmt.Fprintf(os.Stderr, "Usage: %s [options] <pdf-file>\n\n", filepath.Base(os.Args[0]))
	fmt.Fprintf(os.Stderr, "Options:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nExamples:\n")
	fmt.Fprintf(os.Stderr, "  %s document.pdf\n", filepath.Base(os.Args[0]))
	fmt.Fprintf(os.Stderr, "  %s -o output.txt document.pdf\n", filepath.Base(os.Args[0]))
	fmt.Fprintf(os.Stderr, "  %s -v document.pdf > output.txt\n", filepath.Base(os.Args[0]))
}
