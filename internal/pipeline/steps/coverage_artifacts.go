package steps

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxCoverageFileBytes bounds how large a single artifact the reader will
// parse. A test command's coverage directory is not otherwise size-limited,
// and a multi-gigabyte profile would hold up the guard reading it into
// memory for no benefit the guard's checks need.
const maxCoverageFileBytes = 64 << 20

// maxCoverageFilesScanned bounds how many recognised artifacts one directory
// walk parses. A coverage directory is expected to hold a handful of them; a
// runaway command dumping thousands should not turn the guard into an
// unbounded parse. Files the walk classifies as neither a profile nor a report
// are not charged against it, because a runner's own output beside its
// artifacts is not the guard's business.
const maxCoverageFilesScanned = 256

// maxCoverageEntriesVisited bounds how many regular files one walk examines at
// all. Every entry costs a 4096-byte prefix read before anything can classify
// it, so a command that dumps an HTML coverage tree of a hundred thousand
// files would otherwise keep the guard reading long after it had everything it
// needs, and maxCoverageFilesScanned never trips because none of those files
// is an artifact.
const maxCoverageEntriesVisited = 4096

// coverageFileSize is the size lookup readCoverageArtifacts uses to enforce
// maxCoverageFileBytes. It is a seam so a test can make a small file appear
// oversized without writing 64 MiB to disk.
var coverageFileSize = func(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// coverageFileKind is what classifyCoverageFile decided a file's content is,
// judged on the bytes rather than the name because a test command is free to
// name its output anything.
type coverageFileKind int

const (
	coverageKindUnknown coverageFileKind = iota
	coverageKindLCOV
	coverageKindCobertura
	coverageKindJUnit
	coverageKindTRX
)

// readCoverageArtifacts walks dir and parses every file it recognises as a
// coverage profile or test report, merging the results. A directory the test
// command never created is the ordinary case for a misconfigured or vacuous
// run, not a failure, so a missing dir returns a zero value rather than an
// error.
//
// Each report's executed count is clamped at zero before it is summed. A
// malformed sibling can report a negative count, and letting it cancel a real
// one raised a missing-test park against a unit that genuinely ran tests.
func readCoverageArtifacts(dir string) (coverageArtifacts, error) {
	var artifacts coverageArtifacts
	seenFiles := map[string]bool{}
	scanned := 0
	visited := 0

	// An I/O fault on one entry is recorded and walked past rather than
	// returned. A test command owns this directory and can leave a mode-000
	// file or an unsearchable subdirectory in it, and failing the run there
	// reports a reporting problem as a pipeline defect; Skipped already exists
	// to carry it to the maintainer park.
	note := func(path string, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		artifacts.Skipped = append(artifacts.Skipped, fmt.Sprintf("%s (%v)", path, err))
		return nil
	}

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				if errors.Is(err, fs.ErrNotExist) {
					return filepath.SkipAll
				}
				// The root gets the same treatment as an entry inside it. The
				// test command owns this directory and can chmod 000 it, which
				// is the unreadable-artifact case the maintainer park exists
				// for, not a pipeline defect that should fail the run.
				artifacts.Skipped = append(artifacts.Skipped, fmt.Sprintf("%s (%v)", dir, err))
				return filepath.SkipAll
			}
			return note(path, err)
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if visited >= maxCoverageEntriesVisited {
			artifacts.ScanLimited = true
			return filepath.SkipAll
		}
		visited++
		prefix, prefixErr := readFilePrefix(path, 4096)
		if prefixErr != nil {
			return note(path, prefixErr)
		}
		charged := false
		kind, classifyErr := classifyCoverageFile(prefix)
		if kind == coverageKindUnknown && classifyErr != nil {
			// The prefix begins like XML but no start element was reachable
			// inside it, which a truncated artifact and a long leading comment
			// both produce. Treating that as "some other file the runner wrote"
			// let a half-written profile clear HasProfile on a sibling's data,
			// so the whole file is read before it is written off, and a file
			// that still yields no root element is recorded rather than ignored.
			//
			// That whole-file read is charged against the scan budget even when
			// it yields nothing, because it is the same work parsing an artifact
			// costs and a directory of truncated XML would otherwise pay it
			// without limit.
			if scanned >= maxCoverageFilesScanned {
				artifacts.ScanLimited = true
				return filepath.SkipAll
			}
			scanned++
			charged = true
			kind, classifyErr = classifyWholeCoverageFile(path)
			if kind == coverageKindUnknown {
				if classifyErr != nil {
					artifacts.Skipped = append(artifacts.Skipped, fmt.Sprintf("%s (XML that could not be tokenized: %v)", path, classifyErr))
				}
				return nil
			}
		}
		if kind == coverageKindUnknown {
			// Classification comes before the scan budget and the size check on
			// purpose. A runner writes whatever it likes beside its artifacts
			// (an HTML report tree, a multi-gigabyte log), and charging those
			// against the artifact budget or reporting them as skipped
			// artifacts made a correctly configured repository park over files
			// the guard never wanted. Only the entry budget above sees them,
			// and it is set high enough that a handful of stray files cannot
			// reach it.
			return nil
		}

		if !charged {
			if scanned >= maxCoverageFilesScanned {
				artifacts.ScanLimited = true
				return filepath.SkipAll
			}
			scanned++
		}

		size, sizeErr := coverageFileSize(path)
		if sizeErr != nil {
			return note(path, sizeErr)
		}
		if size > maxCoverageFileBytes {
			artifacts.Skipped = append(artifacts.Skipped, path)
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return note(path, readErr)
		}

		// The parse error travels with the path. This park is the never
		// auto-fixable one, so the maintainer is the only one who can act on
		// it, and "could not be parsed: <path>" leaves them to rediscover a
		// diagnosis the parser already made.
		unparseable := func(parseErr error) error {
			artifacts.Unparseable = append(artifacts.Unparseable, fmt.Sprintf("%s (%v)", path, parseErr))
			return nil
		}

		switch kind {
		case coverageKindLCOV:
			profile, parseErr := parseLCOV(data)
			if parseErr != nil {
				return unparseable(parseErr)
			}
			mergeCoverageProfile(&artifacts, seenFiles, profile)
			artifacts.HasProfile = true
		case coverageKindCobertura:
			profile, parseErr := parseCobertura(data)
			if parseErr != nil {
				return unparseable(parseErr)
			}
			mergeCoverageProfile(&artifacts, seenFiles, profile)
			artifacts.HasProfile = true
		case coverageKindJUnit:
			report, parseErr := parseJUnitReport(data)
			if parseErr != nil {
				return unparseable(parseErr)
			}
			artifacts.Report.Executed += max(0, report.Executed)
			artifacts.HasReport = true
		case coverageKindTRX:
			report, parseErr := parseTRXReport(data)
			if parseErr != nil {
				return unparseable(parseErr)
			}
			artifacts.Report.Executed += max(0, report.Executed)
			artifacts.HasReport = true
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return coverageArtifacts{}, nil
		}
		return coverageArtifacts{}, walkErr
	}
	return artifacts, nil
}

// mergeCoverageProfile folds src into dst.Profile, deduplicating Files against
// seenFiles which spans the whole walk so a file named by two different
// artifacts still appears once, in the order the walk first met it.
func mergeCoverageProfile(dst *coverageArtifacts, seenFiles map[string]bool, src coverageProfile) {
	dst.Profile.Functions = append(dst.Profile.Functions, src.Functions...)
	for _, file := range src.Files {
		if seenFiles[file] {
			continue
		}
		seenFiles[file] = true
		dst.Profile.Files = append(dst.Profile.Files, file)
	}
}

// readFilePrefix reads up to n bytes from the start of path. A file shorter
// than n is not an error; the caller classifies on whatever was there.
func readFilePrefix(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:read], nil
}

// classifyCoverageFile decides a file's coverage format from its content
// rather than its name, because a test command names its output anything it
// wants. LCOV is a line-oriented text format identified by an "SF:" record;
// the other three are XML, distinguished by root element name.
//
// The error is non-nil only for data that begins like XML and yielded no start
// element, which separates a genuine non-XML file (keep ignoring it) from XML
// the reader could not tokenize (record it, because ignoring a half-read
// artifact lets a sibling's data certify the unit). A well-formed document
// under an unrecognised root is neither: it is somebody else's XML.
func classifyCoverageFile(prefix []byte) (coverageFileKind, error) {
	for _, line := range strings.Split(string(prefix), "\n") {
		if strings.HasPrefix(strings.TrimRight(line, "\r"), "SF:") {
			return coverageKindLCOV, nil
		}
	}
	root, err := xmlRootElementName(prefix)
	if err != nil {
		if beginsAsXML(prefix) {
			return coverageKindUnknown, err
		}
		return coverageKindUnknown, nil
	}
	switch root {
	case "coverage":
		return coverageKindCobertura, nil
	case "testsuites", "testsuite":
		return coverageKindJUnit, nil
	case "TestRun":
		return coverageKindTRX, nil
	default:
		return coverageKindUnknown, nil
	}
}

// beginsAsXML answers whether data opens an XML document, which is the only
// case where a tokenizer failure is evidence of a broken artifact rather than
// of an ordinary file the runner dropped beside its artifacts. The test is the
// FIRST non-whitespace byte after a UTF-8 BOM, not the presence of a '<'
// anywhere in the prefix: jest writes lcov-report/*.js beside lcov.info, and
// one `i < len` in a loop was enough to record the runner's own JavaScript as
// an unreadable artifact, which flipped every later verdict from the
// auto-fixable park to the maintainer one.
func beginsAsXML(data []byte) bool {
	trimmed := bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	trimmed = bytes.TrimLeft(trimmed, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("<"))
}

// classifyWholeCoverageFile re-runs the classification over a file's complete
// contents, for a prefix that began like XML without reaching a start element.
// An oversized file is reported rather than read, because the size limit exists
// exactly so the guard never pulls one into memory.
func classifyWholeCoverageFile(path string) (coverageFileKind, error) {
	size, err := coverageFileSize(path)
	if err != nil {
		return coverageKindUnknown, err
	}
	if size > maxCoverageFileBytes {
		return coverageKindUnknown, fmt.Errorf("%d bytes exceeds the %d byte limit", size, maxCoverageFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return coverageKindUnknown, err
	}
	return classifyCoverageFile(data)
}

// xmlRootElementName scans for the first start element, tolerating an XML
// declaration, a BOM, comments, and leading whitespace, all of which
// encoding/xml already skips as it tokenizes. The decoder's own error is
// returned so a caller can tell malformed XML from a file that is not XML.
func xmlRootElementName(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

// parseLCOV parses an LCOV coverage trace. LCOV has two ways to report a
// function's hit count: the classic FN/FNDA pair, matched by function name,
// and the lcov 2.x FNL/FNA pair, which joins them through a numeric id
// instead so a function can be identified without its name being unique.
func parseLCOV(data []byte) (coverageProfile, error) {
	var profile coverageProfile
	seenFiles := map[string]bool{}
	sawSF := false

	var fnOrder []coveredFunction
	fndaHits := map[string]int{}
	var fndaOrder []string
	fnlLines := map[int]int{}
	type fnaRecord struct {
		id   int
		hits int
		name string
	}
	var fnaOrder []fnaRecord
	currentFile := ""

	flush := func() {
		if currentFile == "" {
			return
		}
		named := map[string]bool{}
		for _, fn := range fnOrder {
			hits := fndaHits[fn.Name]
			named[fn.Name] = true
			profile.Functions = append(profile.Functions, coveredFunction{File: currentFile, Name: fn.Name, Line: fn.Line, Hits: hits})
		}
		for _, name := range fndaOrder {
			if named[name] {
				continue
			}
			named[name] = true
			profile.Functions = append(profile.Functions, coveredFunction{File: currentFile, Name: name, Line: 0, Hits: fndaHits[name]})
		}
		for _, rec := range fnaOrder {
			profile.Functions = append(profile.Functions, coveredFunction{File: currentFile, Name: rec.name, Line: fnlLines[rec.id], Hits: rec.hits})
		}
	}
	reset := func() {
		fnOrder = nil
		fndaHits = map[string]int{}
		fndaOrder = nil
		fnlLines = map[int]int{}
		fnaOrder = nil
	}

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "SF:"):
			flush()
			reset()
			currentFile = strings.TrimPrefix(line, "SF:")
			sawSF = true
			if currentFile == "" {
				continue
			}
			if !seenFiles[currentFile] {
				seenFiles[currentFile] = true
				profile.Files = append(profile.Files, currentFile)
			}
		case strings.HasPrefix(line, "FN:"):
			lineNum, name, err := splitLCOVFunctionRecord(strings.TrimPrefix(line, "FN:"), line)
			if err != nil {
				return coverageProfile{}, err
			}
			fnOrder = append(fnOrder, coveredFunction{Name: name, Line: lineNum})
		case strings.HasPrefix(line, "FNDA:"):
			hits, name, err := splitLCOVIntField(strings.TrimPrefix(line, "FNDA:"), line)
			if err != nil {
				return coverageProfile{}, err
			}
			fndaHits[name] = hits
			fndaOrder = append(fndaOrder, name)
		case strings.HasPrefix(line, "FNL:"):
			parts := strings.SplitN(strings.TrimPrefix(line, "FNL:"), ",", 3)
			if len(parts) < 2 {
				return coverageProfile{}, fmt.Errorf("lcov: malformed FNL record %q", line)
			}
			id, err := strconv.Atoi(parts[0])
			if err != nil {
				return coverageProfile{}, fmt.Errorf("lcov: malformed FNL id in %q: %w", line, err)
			}
			lineNum, err := strconv.Atoi(parts[1])
			if err != nil {
				return coverageProfile{}, fmt.Errorf("lcov: malformed FNL line in %q: %w", line, err)
			}
			fnlLines[id] = lineNum
		case strings.HasPrefix(line, "FNA:"):
			parts := strings.SplitN(strings.TrimPrefix(line, "FNA:"), ",", 3)
			if len(parts) != 3 {
				return coverageProfile{}, fmt.Errorf("lcov: malformed FNA record %q", line)
			}
			id, err := strconv.Atoi(parts[0])
			if err != nil {
				return coverageProfile{}, fmt.Errorf("lcov: malformed FNA id in %q: %w", line, err)
			}
			hits, err := strconv.Atoi(parts[1])
			if err != nil {
				return coverageProfile{}, fmt.Errorf("lcov: malformed FNA hit count in %q: %w", line, err)
			}
			fnaOrder = append(fnaOrder, fnaRecord{id: id, hits: hits, name: parts[2]})
		case line == "end_of_record":
			flush()
			reset()
			currentFile = ""
		}
	}
	flush()

	if !sawSF {
		return coverageProfile{}, errors.New("lcov: no SF record found")
	}
	return profile, nil
}

// splitLCOVFunctionRecord parses an FN record in either of its two shapes:
// lcov 1.x writes FN:<line>,<name>, and lcov 2.x writes
// FN:<start_line>,<end_line>,<name>. Reading the 2.x form as the 1.x one gave
// the function the name "<end_line>,<name>", which then matched no FNDA
// record, while the real name arrived through the flush fallback with no
// declaration line and so spanned the whole file. Every executed function in
// that file then satisfied the changed-line intersection, which silently
// downgraded the per-function check to a per-file one.
func splitLCOVFunctionRecord(field string, fullLine string) (int, string, error) {
	if parts := strings.SplitN(field, ",", 3); len(parts) == 3 {
		start, startErr := strconv.Atoi(parts[0])
		_, endErr := strconv.Atoi(parts[1])
		if startErr == nil && endErr == nil {
			return start, parts[2], nil
		}
	}
	return splitLCOVIntField(field, fullLine)
}

// splitLCOVIntField parses the "<int>,<name>" shape shared by FN and FNDA
// records. fullLine is only for the error message.
func splitLCOVIntField(field string, fullLine string) (int, string, error) {
	parts := strings.SplitN(field, ",", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("lcov: malformed record %q", fullLine)
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", fmt.Errorf("lcov: malformed number in %q: %w", fullLine, err)
	}
	return n, parts[1], nil
}

type coberturaCoverage struct {
	XMLName  xml.Name           `xml:"coverage"`
	Packages []coberturaPackage `xml:"packages>package"`
}

type coberturaPackage struct {
	Classes []coberturaClass `xml:"classes>class"`
}

type coberturaClass struct {
	Filename string            `xml:"filename,attr"`
	Methods  []coberturaMethod `xml:"methods>method"`
}

type coberturaMethod struct {
	Name     string          `xml:"name,attr"`
	LineRate string          `xml:"line-rate,attr"`
	Lines    []coberturaLine `xml:"lines>line"`
}

type coberturaLine struct {
	Number int `xml:"number,attr"`
	Hits   int `xml:"hits,attr"`
}

// parseCobertura parses a Cobertura XML coverage report. A method with no
// <line> children (a declaration Cobertura could not attribute to a source
// line) still needs a hit verdict, so it borrows one from its line-rate: any
// rate above zero means the method executed.
func parseCobertura(data []byte) (coverageProfile, error) {
	var doc coberturaCoverage
	if err := xml.Unmarshal(data, &doc); err != nil {
		return coverageProfile{}, fmt.Errorf("cobertura: %w", err)
	}
	var profile coverageProfile
	seenFiles := map[string]bool{}
	for _, pkg := range doc.Packages {
		for _, class := range pkg.Classes {
			// A class with no filename names no file. Admitting "" would let a
			// profile that describes nothing pass the "profile describing
			// nothing" maintainer park and seed an empty extension that every
			// extensionless changed file then matches.
			if class.Filename == "" {
				continue
			}
			if !seenFiles[class.Filename] {
				seenFiles[class.Filename] = true
				profile.Files = append(profile.Files, class.Filename)
			}
			for _, method := range class.Methods {
				if len(method.Lines) == 0 {
					hits := 0
					if method.LineRate != "" {
						rate, err := strconv.ParseFloat(method.LineRate, 64)
						if err != nil {
							return coverageProfile{}, fmt.Errorf("cobertura: malformed line-rate attribute %q: %w", method.LineRate, err)
						}
						if rate > 0 {
							hits = 1
						}
					}
					profile.Functions = append(profile.Functions, coveredFunction{File: class.Filename, Name: method.Name, Line: 0, Hits: hits})
					continue
				}
				line, hits := method.Lines[0].Number, method.Lines[0].Hits
				for _, l := range method.Lines[1:] {
					if l.Number < line {
						line = l.Number
					}
					if l.Hits > hits {
						hits = l.Hits
					}
				}
				profile.Functions = append(profile.Functions, coveredFunction{File: class.Filename, Name: method.Name, Line: line, Hits: hits})
			}
		}
	}
	return profile, nil
}

// Skipped is a pointer because an absent root skipped attribute and an
// explicit skipped="0" mean different things: jest-junit emits the root count
// as tests="4" with no root skipped attribute and carries skipped="4" on the
// child suite, so reading the absent attribute as zero greened a run whose
// every test was skipped.
type junitTestsuites struct {
	XMLName xml.Name         `xml:"testsuites"`
	Tests   string           `xml:"tests,attr"`
	Skipped *string          `xml:"skipped,attr"`
	Suites  []junitTestsuite `xml:"testsuite"`
}

type junitTestsuite struct {
	Tests   string `xml:"tests,attr"`
	Skipped string `xml:"skipped,attr"`
}

// parseJUnitReport parses a JUnit XML test report. Executed excludes skipped
// tests, and the root testsuites element's own tests attribute wins over
// summing its children: a runner that reports both means the root total for
// the whole file, and adding the children on top would double count. A root
// total with no root skipped attribute takes its skipped count from the sum of
// the children instead.
func parseJUnitReport(data []byte) (testReport, error) {
	root, err := xmlRootElementName(data)
	if err != nil {
		return testReport{}, fmt.Errorf("junit: no root element found: %w", err)
	}
	switch root {
	case "testsuites":
		var doc junitTestsuites
		if err := xml.Unmarshal(data, &doc); err != nil {
			return testReport{}, fmt.Errorf("junit: %w", err)
		}
		if doc.Tests != "" {
			tests, err := junitCount("tests", doc.Tests)
			if err != nil {
				return testReport{}, err
			}
			skipped := 0
			if doc.Skipped != nil {
				skipped, err = junitCount("skipped", *doc.Skipped)
				if err != nil {
					return testReport{}, err
				}
			} else {
				for _, suite := range doc.Suites {
					n, err := junitCount("skipped", suite.Skipped)
					if err != nil {
						return testReport{}, err
					}
					skipped += n
				}
			}
			return junitExecuted(tests, skipped)
		}
		total := 0
		for _, suite := range doc.Suites {
			tests, err := junitCount("tests", suite.Tests)
			if err != nil {
				return testReport{}, err
			}
			skipped, err := junitCount("skipped", suite.Skipped)
			if err != nil {
				return testReport{}, err
			}
			report, err := junitExecuted(tests, skipped)
			if err != nil {
				return testReport{}, err
			}
			total += report.Executed
		}
		return testReport{Executed: total}, nil
	case "testsuite":
		var doc junitTestsuite
		if err := xml.Unmarshal(data, &doc); err != nil {
			return testReport{}, fmt.Errorf("junit: %w", err)
		}
		tests, err := junitCount("tests", doc.Tests)
		if err != nil {
			return testReport{}, err
		}
		skipped, err := junitCount("skipped", doc.Skipped)
		if err != nil {
			return testReport{}, err
		}
		return junitExecuted(tests, skipped)
	default:
		return testReport{}, fmt.Errorf("junit: unrecognised root element %q", root)
	}
}

// junitExecuted subtracts the skipped count from the tests count, and reports
// a negative result as unparseable rather than clamping it. A report claiming
// more skipped tests than tests is a reporting defect only the maintainer can
// fix, and a silent clamp to zero handed it to the auto-fixable park instead,
// charging an agent fix round with writing a test for a counter bug. The clamp
// where the reports are summed stays, as defense for the multi-report case.
func junitExecuted(tests, skipped int) (testReport, error) {
	if tests-skipped < 0 {
		return testReport{}, fmt.Errorf("junit: skipped count %d exceeds tests count %d", skipped, tests)
	}
	return testReport{Executed: tests - skipped}, nil
}

// junitCount reads one JUnit counter attribute. An absent attribute is zero,
// but a malformed one is an error rather than a silent zero: swallowing it
// either inflates the executed count and greens a suite whose every test was
// filtered out, or drives it to zero and charges an agent fix round with a
// reporting defect that belongs to the maintainer.
func junitCount(name, value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("junit: malformed %s attribute %q: %w", name, value, err)
	}
	return n, nil
}

type trxTestRun struct {
	XMLName       xml.Name         `xml:"TestRun"`
	ResultSummary trxResultSummary `xml:"ResultSummary"`
}

type trxResultSummary struct {
	Counters trxCounters `xml:"Counters"`
}

type trxCounters struct {
	Executed string `xml:"executed,attr"`
}

// parseTRXReport parses a Visual Studio TRX test report. Only the executed
// counter answers "did anything run": VSTest's total counts every test the run
// discovered, including the ones it filtered out or never scheduled, so a run
// that executed nothing still reports a nonzero total. A file without the
// executed counter is unparseable, which parks for the maintainer to fix the
// reporting rather than charging an agent with a missing test.
func parseTRXReport(data []byte) (testReport, error) {
	var doc trxTestRun
	if err := xml.Unmarshal(data, &doc); err != nil {
		return testReport{}, fmt.Errorf("trx: %w", err)
	}
	counters := doc.ResultSummary.Counters
	if counters.Executed == "" {
		return testReport{}, errors.New("trx: Counters has no executed attribute")
	}
	n, err := strconv.Atoi(counters.Executed)
	if err != nil {
		return testReport{}, fmt.Errorf("trx: malformed executed attribute %q: %w", counters.Executed, err)
	}
	if n < 0 {
		return testReport{}, fmt.Errorf("trx: negative executed count %d", n)
	}
	return testReport{Executed: n}, nil
}
