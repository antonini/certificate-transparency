package scanner

import (
	"bytes"
	"container/list"
	"crypto/x509"
	"ct/client"
	"fmt"
	"log"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// Function prototype for matcher functions:
type Matcher interface {
	CertificateMatches(*x509.Certificate) bool
}

type MatchAll struct{}

func (m MatchAll) CertificateMatches(_ *x509.Certificate) bool {
	return true
}

type MatchNone struct{}

func (m MatchNone) CertificateMatches(_ *x509.Certificate) bool {
	return false
}

type MatchSubjectRegex struct {
	SubjectRegex *regexp.Regexp
}

func (m MatchSubjectRegex) CertificateMatches(c *x509.Certificate) bool {
	if m.SubjectRegex.FindStringIndex(c.Subject.CommonName) != nil {
		return true
	}
	for _, alt := range c.DNSNames {
		if m.SubjectRegex.FindStringIndex(alt) != nil {
			return true
		}
	}
	return false
}

// ScannerOptions holds configuration options for the Scanner
type ScannerOptions struct {
	// Custom matcher for x509 Certificates, functor will be called for each
	// Certificate found during scanning.
	Matcher Matcher

	// Number of entries to request in one batch from the Log
	BlockSize int

	// Number of concurrent matchers to run
	NumWorkers int

	// Number of concurrent fethers to run
	ParallelFetch int

	// Log entry index to start fetching & matching at
	StartIndex int64
}

// Scanner is a tool to scan all the entries in a CT Log.
type Scanner struct {
	// Client used to talk to the CT log instance
	logClient *client.LogClient

	// Configuration options for this Scanner instance
	opts ScannerOptions

	// Counter of the number of certificates scanned
	certsProcessed int64

	// Counter of the number of precertificates encountered during the scan.
	precertsSeen int64
}

// matcherJob represents the context for an individual matcher job.
type matcherJob struct {
	// The raw LeafInput returned by the log server
	leaf client.LeafInput
	// The index of the entry containing the LeafInput in the log
	index int64
}

// fetchRange represents a range of certs to fetch from a CT log
type fetchRange struct {
	start int64
	end   int64
}

// Processes the given |leafInput| found at |index| in the specified log.
func (s *Scanner) processEntry(index int64, leafInput client.LeafInput, foundCert func(int64, *x509.Certificate), foundPrecert func(int64, string)) {
	atomic.AddInt64(&s.certsProcessed, 1)
	leaf, err := client.NewMerkleTreeLeaf(bytes.NewBuffer(leafInput))
	if err != nil {
		log.Printf("Failed to parse MerkleTreeLeaf at index %d : %s", index, err.Error())
		return
	}
	switch leaf.TimestampedEntry.EntryType {
	case client.X509LogEntryType:
		cert, err := x509.ParseCertificate(leaf.TimestampedEntry.X509Entry)
		if err != nil {
			log.Printf("Failed to parse cert at index %d : %s", index, err.Error())
			return
		}
		if s.opts.Matcher.CertificateMatches(cert) {
			foundCert(index, cert)
		}
	case client.PrecertLogEntryType:
		log.Printf("Precert not yet supported (index %d).", index)
		foundPrecert(index, "")
		s.precertsSeen++
	}
}

// Worker function to match certs.
// Accepts MatcherJobs over the |entries| channel, and processes them.
// Returns true over the |done| channel when the |entries| channel is closed.
func (s *Scanner) matcherJob(id int, entries <-chan matcherJob, foundCert func(int64, *x509.Certificate), foundPrecert func(int64, string), wg *sync.WaitGroup) {
	for e := range entries {
		s.processEntry(e.index, e.leaf, foundCert, foundPrecert)
	}
	log.Printf("Matcher %d finished", id)
	wg.Done()
}

// Worker function for fetcher jobs.
// Accepts cert ranges to fetch over the |ranges| channel, and if the fetch is
// successful sends the individual LeafInputs out (as MatcherJobs) into the
// |entries| channel for the matchers to chew on.
// Will retry failed attempts to retrieve ranges indefinitely.
// Sends true over the |done| channel when the |ranges| channel is closed.
func (s *Scanner) fetcherJob(id int, ranges <-chan fetchRange, entries chan<- matcherJob, wg *sync.WaitGroup) {
	for r := range ranges {
		success := false
		// TODO(alcutter): give up after a while:
		for !success {
			leaves, err := s.logClient.GetEntries(r.start, r.end)
			if err != nil {
				log.Printf("Problem fetching from log: %s", err.Error())
				continue
			}
			for _, leaf := range leaves {
				entries <- matcherJob{leaf, r.start}
				r.start++
			}
			if r.start > r.end {
				// Only complete if we actually got all the leaves we were
				// expecting -- Logs MAY return fewer than the number of
				// leaves requested.
				success = true
			}
		}
	}
	log.Printf("Fetcher %d finished", id)
	wg.Done()
}

// Returns the smaller of |a| and |b|
func min(a int64, b int64) int64 {
	if a < b {
		return a
	} else {
		return b
	}
}

// Returns the larger of |a| and |b|
func max(a int64, b int64) int64 {
	if a > b {
		return a
	} else {
		return b
	}
}

// Pretty prints the passed in number of |seconds| into a more human readable
// string.
func humanTime(seconds int) string {
	nanos := time.Duration(seconds) * time.Second
	hours := int(nanos / (time.Hour))
	nanos %= time.Hour
	minutes := int(nanos / time.Minute)
	nanos %= time.Minute
	seconds = int(nanos / time.Second)
	s := ""
	if hours > 0 {
		s += fmt.Sprintf("%d hours ", hours)
	}
	if minutes > 0 {
		s += fmt.Sprintf("%d minutes ", minutes)
	}
	if seconds > 0 {
		s += fmt.Sprintf("%d seconds ", seconds)
	}
	return s
}

// Performs a scan against the Log.
// For each x509 certificate found, |foundCert| will be called with the
// index of the entry and certificate itself as arguments.  For each precert
// found, |foundPrecert| will be called with the index of the entry and the raw
// precert string as the arguments.
//
// This method blocks until the scan is complete.
func (s *Scanner) Scan(foundCert func(int64, *x509.Certificate), foundPrecert func(int64, string)) error {
	log.Printf("Starting up...\n")
	s.certsProcessed = 0
	s.precertsSeen = 0

	latestSth, err := s.logClient.GetSTH()
	if err != nil {
		return err
	}
	log.Printf("Got STH with %d certs", latestSth.TreeSize)

	ticker := time.NewTicker(time.Second)
	startTime := time.Now()
	go func() {
		for _ = range ticker.C {
			throughput := float64(s.certsProcessed) / time.Since(startTime).Seconds()
			remainingCerts := int64(latestSth.TreeSize) - int64(s.opts.StartIndex) - s.certsProcessed
			remainingSeconds := int(float64(remainingCerts) / throughput)
			remainingString := humanTime(remainingSeconds)
			fmt.Printf("Processed: %d certs (to index %d). Throughput: %3.2f ETA: %s\n",
				s.certsProcessed, s.opts.StartIndex+int64(s.certsProcessed), throughput, remainingString)
		}
	}()

	var ranges list.List
	for start := s.opts.StartIndex; start < int64(latestSth.TreeSize); {
		end := min(start+int64(s.opts.BlockSize), int64(latestSth.TreeSize)-1)
		ranges.PushBack(fetchRange{start, end})
		start = end + 1
	}
	fetches := make(chan fetchRange, 100)
	jobs := make(chan matcherJob, 5000)
	var fetcherWG sync.WaitGroup
	var matcherWG sync.WaitGroup
	// Start matcher workers
	for w := 0; w < s.opts.NumWorkers; w++ {
		matcherWG.Add(1)
		go s.matcherJob(w, jobs, foundCert, foundPrecert, &matcherWG)
	}
	// Start fetcher workers
	for w := 0; w < s.opts.ParallelFetch; w++ {
		fetcherWG.Add(1)
		go s.fetcherJob(w, fetches, jobs, &fetcherWG)
	}
	for r := ranges.Front(); r != nil; r = r.Next() {
		fetches <- r.Value.(fetchRange)
	}
	close(fetches)
	fetcherWG.Wait()
	close(jobs)
	matcherWG.Wait()

	log.Printf("Completed %d certs in %s", s.certsProcessed, humanTime(int(time.Since(startTime).Seconds())))
	log.Printf("Saw %d precerts", s.precertsSeen)
	return nil
}

// Creates a new Scanner instance using |client| to talk to the log, and taking
// configuration options from |opts|.
func NewScanner(client *client.LogClient, opts ScannerOptions) *Scanner {
	var scanner Scanner
	scanner.logClient = client
	// Set a default match-everything regex if none was provided:
	if opts.Matcher == nil {
		opts.Matcher = &MatchAll{}
	}
	scanner.opts = opts
	return &scanner
}