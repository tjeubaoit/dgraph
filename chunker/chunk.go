/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package chunker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	encjson "encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/dgraph-io/dgraph/lex"
	"github.com/dgraph-io/dgraph/x"

	"github.com/pkg/errors"
)

// Chunker describes the interface to parse and process the input to the live and bulk loaders.
type Chunker interface {
	Chunk(r *bufio.Reader) (*bytes.Buffer, error)
	Parse(chunkBuf *bytes.Buffer) error
	NQuads() *NQuadBuffer
}

type rdfChunker struct {
	lexer *lex.Lexer
	nqs   *NQuadBuffer
}

func (rc *rdfChunker) NQuads() *NQuadBuffer {
	return rc.nqs
}

type jsonChunker struct {
	nqs    *NQuadBuffer
	inList bool
}

func (jc *jsonChunker) NQuads() *NQuadBuffer {
	return jc.nqs
}

// InputFormat represents the multiple formats supported by Chunker.
type InputFormat byte

const (
	// UnknownFormat is a constant to denote a format not supported by the bulk/live loaders.
	UnknownFormat InputFormat = iota
	// RdfFormat is a constant to denote the input to the live/bulk loader is in the RDF format.
	RdfFormat
	// JsonFormat is a constant to denote the input to the live/bulk loader is in the JSON format.
	JsonFormat
)

// NewChunker returns a new chunker for the specified format.
func NewChunker(inputFormat InputFormat, batchSize int) Chunker {
	switch inputFormat {
	case RdfFormat:
		return &rdfChunker{
			nqs:   NewNQuadBuffer(batchSize),
			lexer: &lex.Lexer{},
		}
	case JsonFormat:
		return &jsonChunker{
			nqs: NewNQuadBuffer(batchSize),
		}
	default:
		panic("unknown input format")
	}
}

// Chunk reads the input line by line until one of the following 3 conditions happens
// 1) the EOF is reached
// 2) 1e5 lines have been read
// 3) some unexpected error happened
func (*rdfChunker) Chunk(r *bufio.Reader) (*bytes.Buffer, error) {
	batch := new(bytes.Buffer)
	batch.Grow(1 << 20)
	for lineCount := 0; lineCount < 1e5; lineCount++ {
		slc, err := r.ReadSlice('\n')
		if err == io.EOF {
			x.Check2(batch.Write(slc))
			return batch, err
		}
		if err == bufio.ErrBufferFull {
			// This should only happen infrequently.
			x.Check2(batch.Write(slc))
			var str string
			str, err = r.ReadString('\n')
			if err == io.EOF {
				x.Check2(batch.WriteString(str))
				return batch, err
			}
			if err != nil {
				return nil, err
			}
			x.Check2(batch.WriteString(str))
			continue
		}
		if err != nil {
			return nil, err
		}
		x.Check2(batch.Write(slc))
	}
	return batch, nil
}

// Parse is not thread-safe. Only call it serially, because it reuses lexer object.
func (rc *rdfChunker) Parse(chunkBuf *bytes.Buffer) error {
	if chunkBuf == nil || chunkBuf.Len() == 0 {
		return nil
	}

	for chunkBuf.Len() > 0 {
		str, err := chunkBuf.ReadString('\n')
		if err != nil && err != io.EOF {
			x.Check(err)
		}

		nq, err := ParseRDF(str, rc.lexer)
		if err == ErrEmpty {
			continue // blank line or comment
		} else if err != nil {
			return errors.Wrapf(err, "while parsing line %q", str)
		}
		rc.nqs.Push(&nq)
	}
	return nil
}

// Chunk tries to consume multiple top-level maps from the reader until a size threshold is
// reached, or the end of file is reached.
func (jc *jsonChunker) Chunk(r *bufio.Reader) (*bytes.Buffer, error) {
	ch, err := jc.nextRune(r)
	if err != nil {
		return nil, err
	}
	// If the file starts with a list rune [, we set the inList flag, and keep consuming maps
	// until we reach the threshold.
	if ch == '[' {
		jc.inList = true
	} else if ch == '{' {
		// put the rune back for it to be consumed in the consumeMap function
		if err := r.UnreadRune(); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.Errorf("file is not JSON")
	}

	out := new(bytes.Buffer)
	x.Check2(out.WriteRune('['))
	hasMapsBefore := false
	for out.Len() < 1e5 {
		if hasMapsBefore {
			x.Check2(out.WriteRune(','))
		}
		if err := jc.consumeMap(r, out); err != nil {
			return nil, err
		}
		hasMapsBefore = true

		// handle the legal termination cases, by checking the next rune after the map
		ch, err := jc.nextRune(r)
		if err == io.EOF {
			// handles the EOF case, return the buffer which represents the top level map
			if jc.inList {
				return nil, errors.Errorf("JSON file ends abruptly, expecting ]")
			}

			x.Check2(out.WriteRune(']'))
			return out, io.EOF
		} else if err != nil {
			return nil, err
		}

		if ch == ']' {
			if !jc.inList {
				return nil, errors.Errorf("JSON map is followed by an extraneous ]")
			}

			// validate that there are no more non-space chars after the ]
			if slurpSpace(r) != io.EOF {
				return nil, errors.New("Not all of JSON file consumed")
			}

			x.Check2(out.WriteRune(']'))
			return out, io.EOF
		}

		// In the non termination cases, ensure at least one map has been consumed, and
		// the only allowed char after the map is ",".
		if out.Len() == 1 { // 1 represents the [ inserted before the for loop
			return nil, errors.Errorf("Illegal rune found \"%c\", expecting {", ch)
		}
		if ch != ',' {
			return nil, errors.Errorf("JSON map is followed by illegal rune \"%c\"", ch)
		}
	}
	x.Check2(out.WriteRune(']'))
	return out, nil
}

// consumeMap consumes the next map from the reader, and stores the result into the buffer out.
// After ignoring spaces, if the reader does not begin with {, no rune will be consumed
// from the reader.
func (jc *jsonChunker) consumeMap(r *bufio.Reader, out *bytes.Buffer) error {
	// Just find the matching closing brace. Let the JSON-to-nquad parser in the mapper worry
	// about whether everything in between is valid JSON or not.
	depth := 0
	for {
		ch, err := jc.nextRune(r)
		if err != nil {
			return errors.New("Malformed JSON")
		}
		if depth == 0 && ch != '{' {
			// We encountered a beginning rune that's not {,
			// unread the char and return without consuming anything.
			if err := r.UnreadRune(); err != nil {
				return err
			}
			return nil
		}

		x.Check2(out.WriteRune(ch))
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
		case '"':
			if err := slurpQuoted(r, out); err != nil {
				return err
			}
		default:
			// We just write the rune to out, and let the Go JSON parser do its job.
		}
		if depth <= 0 {
			break
		}
	}
	return nil
}

// nextRune ignores any number of spaces that may precede a rune
func (*jsonChunker) nextRune(r *bufio.Reader) (rune, error) {
	if err := slurpSpace(r); err != nil {
		return ' ', err
	}
	ch, _, err := r.ReadRune()
	if err != nil {
		return ' ', err
	}
	return ch, nil
}

func (jc *jsonChunker) Parse(chunkBuf *bytes.Buffer) error {
	if chunkBuf == nil || chunkBuf.Len() == 0 {
		return nil
	}

	err := jc.nqs.ParseJSON(chunkBuf.Bytes(), SetNquads)
	return err
}

func slurpSpace(r *bufio.Reader) error {
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return err
		}
		if !unicode.IsSpace(ch) {
			x.Check(r.UnreadRune())
			return nil
		}
	}
}

func slurpQuoted(r *bufio.Reader, out *bytes.Buffer) error {
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return err
		}
		x.Check2(out.WriteRune(ch))

		if ch == '\\' {
			// Pick one more rune.
			esc, _, err := r.ReadRune()
			if err != nil {
				return err
			}
			x.Check2(out.WriteRune(esc))
			continue
		}
		if ch == '"' {
			return nil
		}
	}
}

// FileReader returns an open reader and file on the given file. Gzip-compressed input is detected
// and decompressed automatically even without the gz extension. The caller is responsible for
// calling the returned cleanup function when done with the reader.
func FileReader(file string) (rd *bufio.Reader, cleanup func()) {
	var f *os.File
	var err error
	if file == "-" {
		f = os.Stdin
	} else {
		f, err = os.Open(file)
	}

	x.Check(err)

	cleanup = func() { f.Close() }

	if filepath.Ext(file) == ".gz" {
		gzr, err := gzip.NewReader(f)
		x.Check(err)
		rd = bufio.NewReader(gzr)
		cleanup = func() { f.Close(); gzr.Close() }
	} else {
		rd = bufio.NewReader(f)
		buf, _ := rd.Peek(512)

		typ := http.DetectContentType(buf)
		if typ == "application/x-gzip" {
			gzr, err := gzip.NewReader(rd)
			x.Check(err)
			rd = bufio.NewReader(gzr)
			cleanup = func() { f.Close(); gzr.Close() }
		}
	}

	return rd, cleanup
}

// IsJSONData returns true if the reader, which should be at the start of the stream, is reading
// a JSON stream, false otherwise.
func IsJSONData(r *bufio.Reader) (bool, error) {
	buf, err := r.Peek(512)
	if err != nil && err != io.EOF {
		return false, err
	}

	de := encjson.NewDecoder(bytes.NewReader(buf))
	_, err = de.Token()

	return err == nil, nil
}

// DataFormat returns a file's data format (RDF, JSON, or unknown) based on the filename
// or the user-provided format option. The file extension has precedence.
func DataFormat(filename string, format string) InputFormat {
	format = strings.ToLower(format)
	filename = strings.TrimSuffix(strings.ToLower(filename), ".gz")
	switch {
	case strings.HasSuffix(filename, ".rdf") || format == "rdf":
		return RdfFormat
	case strings.HasSuffix(filename, ".json") || format == "json":
		return JsonFormat
	default:
		return UnknownFormat
	}
}
