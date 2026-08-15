package main // Defines this file as an executable Go program (package main means it produces a runnable binary).

import ( // Starts the list of standard-library packages this program depends on.
	"context"       // Lets us cancel in-flight work cleanly on Ctrl+C or a termination signal.
	"crypto/sha256" // Computes SHA-256 checksums so we can verify a downloaded PDF wasn't corrupted on the way to disk.
	"encoding/hex"  // Renders raw checksum bytes as a human-readable hexadecimal string for logging and comparison.
	"encoding/json" // Decodes JSON responses returned by the LibreTexts catalog API.
	"fmt"           // Formats strings for log messages and error messages.
	"io"            // Streams downloaded PDF bytes from the HTTP response into a file on disk.
	"log"           // Provides timestamped log output to the terminal.
	"math/rand"     // Adds random jitter to retry backoff delays so retries don't line up in lockstep.
	"net/http"      // Performs HTTP requests against the catalog API and the PDF download URLs.
	"net/url"       // Builds and edits URLs, including their query-string parameters.
	"os"            // Handles files, directories, and OS-level process signals.
	"os/signal"     // Lets us listen for Ctrl+C / termination signals and turn them into context cancellation.
	"path/filepath" // Builds filesystem paths in a way that works across operating systems.
	"strconv"       // Converts numbers (like page numbers) to strings for use in URLs.
	"sync/atomic"   // Provides counters that are safe to update without a separate lock.
	"syscall"       // Names the SIGTERM signal so we can listen for a graceful-shutdown request.
	"time"          // Provides timeouts, delays, and elapsed-time measurements.
) // Ends the import list.

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------
//
// Every setting below is a fixed constant on purpose: this program is meant
// to be run with no command-line flags or arguments. To change how it
// behaves, edit the constant values in this block and rebuild the program —
// nothing is read from the command line at runtime.

const ( // Starts the block of fixed configuration constants.
	libreTextsBaseURL      = "https://commons.libretexts.org"                                                                                 // Base URL of the LibreTexts Commons website and API.
	pdfOutputDirectory     = "PDFs"                                                                                                           // Directory (relative to the working directory) where downloaded PDFs are saved.
	booksPerCatalogPage    = 100                                                                                                              // Number of books requested from the API in each catalog page.
	maximumRetryAttempts   = 3                                                                                                                // How many extra attempts to make after an initial failed request, before giving up.
	retryBackoffBaseDelay  = 2 * time.Second                                                                                                  // Starting delay used to compute exponential backoff between retries.
	httpRequestTimeout     = 2 * time.Minute                                                                                                  // Maximum time allowed for any single HTTP request to complete.
	firstCatalogPageNumber = 1                                                                                                                // Catalog page number the program starts requesting from.
	httpUserAgent          = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36" // User-Agent header sent with every outgoing HTTP request.
	pauseAfterEachDownload = 3 * time.Second                                                                                                  // How long to pause after each successful PDF download, to be gentle on the server.
) // Ends the configuration constants.

// runConfiguration bundles the constants above into a single struct so the
// rest of the program (fetchCatalogPage, downloadBookPDF, and friends) can
// take one argument instead of a long list of parameters. It is built once
// at startup from the constants above and never changes during a run.
type runConfiguration struct { // Starts the runConfiguration structure.
	baseURL               string        // Base URL of the LibreTexts Commons API.
	outputDirectory       string        // Directory where downloaded PDFs are written.
	booksPerPage          int           // Number of books requested per catalog page.
	maximumRetries        int           // Maximum number of retry attempts for a failed request or download.
	retryBackoffBaseDelay time.Duration // Base delay used to compute exponential backoff between retries.
	httpTimeout           time.Duration // Per-request HTTP timeout.
	startingPageNumber    int           // Catalog page number to start from (useful for resuming a large run).
	userAgent             string        // User-Agent header sent with every HTTP request.
	pauseAfterDownload    time.Duration // How long to pause after each successful download.
} // Ends the runConfiguration structure.

// buildRunConfiguration copies the fixed constants above into a
// runConfiguration value that the rest of the program can pass around.
func buildRunConfiguration() runConfiguration { // Starts the configuration-builder function.
	return runConfiguration{ // Returns a populated runConfiguration literal.
		baseURL:               libreTextsBaseURL,      // Copies the base URL constant.
		outputDirectory:       pdfOutputDirectory,     // Copies the output-directory constant.
		booksPerPage:          booksPerCatalogPage,    // Copies the books-per-page constant.
		maximumRetries:        maximumRetryAttempts,   // Copies the max-retries constant.
		retryBackoffBaseDelay: retryBackoffBaseDelay,  // Copies the retry-backoff base-delay constant.
		httpTimeout:           httpRequestTimeout,     // Copies the HTTP timeout constant.
		startingPageNumber:    firstCatalogPageNumber, // Copies the starting-page constant.
		userAgent:             httpUserAgent,          // Copies the User-Agent constant.
		pauseAfterDownload:    pauseAfterEachDownload, // Copies the post-download-pause constant.
	} // Ends the runConfiguration literal.
} // Ends buildRunConfiguration.

// ---------------------------------------------------------------------------
// API response types
// ---------------------------------------------------------------------------

// catalogResponse mirrors the JSON shape returned by the LibreTexts
// "commons/catalog" API endpoint for a single page of results.
type catalogResponse struct { // Defines the structure of one catalog API response.
	HasError       bool          `json:"err"`      // True if the API itself reported an error for this request.
	TotalBookCount int           `json:"numTotal"` // Total number of books available across all pages, as reported by the API.
	Books          []catalogBook `json:"books"`    // The books included on this particular page.
	Seed           int           `json:"seed"`     // A seed value returned by the API; not used by this program.
} // Ends the catalogResponse structure.

// catalogBook holds the fields of a single book entry that this program
// actually needs from the catalog API response.
type catalogBook struct { // Defines the book information this program needs.
	BookID string   `json:"bookID"` // The unique LibreTexts identifier for this book.
	Links  struct { // Defines the book's available download links.
		PDF string `json:"pdf"` // Direct URL to download this book as a PDF, if one exists.
	} `json:"links"` // Maps to the JSON "links" object for this book.
} // Ends the catalogBook structure.

// ---------------------------------------------------------------------------
// Run-wide statistics
// ---------------------------------------------------------------------------

// runStatistics collects counters describing what happened during the run.
// atomic.Int64 fields are safe to update without a separate lock, which
// matters if this program is ever changed back to run downloads in parallel.
type runStatistics struct { // Starts the runStatistics structure.
	booksDownloaded   atomic.Int64 // Counts PDFs successfully downloaded this run.
	booksSkipped      atomic.Int64 // Counts PDFs skipped because a valid copy already existed on disk.
	booksWithNoPDF    atomic.Int64 // Counts books that had no PDF download link at all.
	booksFailed       atomic.Int64 // Counts downloads that failed even after all retries.
	totalBytesWritten atomic.Int64 // Counts total bytes written to disk this run, across all downloaded PDFs.
} // Ends the runStatistics structure.

// summary formats a one-line, human-readable snapshot of the current
// statistics. It is used both for periodic progress logs and the final
// end-of-run report.
func (statistics *runStatistics) summary() string { // Starts the summary method.
	return fmt.Sprintf( // Builds the formatted summary string.
		"downloaded=%d skipped=%d no-pdf=%d failed=%d bytes=%s",       // Defines the format template.
		statistics.booksDownloaded.Load(),                             // Supplies the current downloaded count.
		statistics.booksSkipped.Load(),                                // Supplies the current skipped count.
		statistics.booksWithNoPDF.Load(),                              // Supplies the current no-PDF count.
		statistics.booksFailed.Load(),                                 // Supplies the current failed count.
		formatByteCountForHumans(statistics.totalBytesWritten.Load()), // Supplies the formatted total-bytes value.
	) // Ends the Sprintf call.
} // Ends the summary method.

// formatByteCountForHumans renders a raw byte count as a friendly string
// like "12.3 MiB" instead of a large, hard-to-read number of bytes.
func formatByteCountForHumans(byteCount int64) string { // Starts the byte-formatting helper function.
	const bytesPerUnit = 1024 // Defines the base unit for binary byte prefixes (1 KiB = 1024 bytes).

	if byteCount < bytesPerUnit { // Checks whether the count is small enough to show as raw bytes.
		return fmt.Sprintf("%d B", byteCount) // Returns the raw byte count with a "B" suffix.
	} // Ends the small-count check.

	divisor, unitIndex := int64(bytesPerUnit), 0                                                      // Starts the divisor at 1 KiB and the unit index at "K".
	for remaining := byteCount / bytesPerUnit; remaining >= bytesPerUnit; remaining /= bytesPerUnit { // Walks up through KiB, MiB, GiB, and so on.
		divisor *= bytesPerUnit // Grows the divisor by one more unit each time through the loop.
		unitIndex++             // Advances to the next unit prefix letter.
	} // Ends the unit-scaling loop.

	unitLetters := "KMGTPE"                                                                      // Lists the unit-prefix letters in order: Kilo, Mega, Giga, Tera, Peta, Exa.
	return fmt.Sprintf("%.1f %ciB", float64(byteCount)/float64(divisor), unitLetters[unitIndex]) // Formats the value using the matched unit prefix, e.g. "12.3 MiB".
} // Ends formatByteCountForHumans.

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() { // Starts the program.
	runStartTime := time.Now()               // Records when the run began, used to log total elapsed time at the end.
	configuration := buildRunConfiguration() // Builds the run configuration from the fixed constants declared above.

	log.SetFlags(log.LstdFlags | log.Lmsgprefix) // Makes sure every log line is prefixed with a timestamp.
	log.Printf(                                  // Logs the effective configuration once, up front, so a saved log file is self-describing.
		"starting run: baseURL=%s outputDirectory=%s booksPerPage=%d maximumRetries=%d startingPageNumber=%d pauseAfterDownload=%s", // Defines the log message template.
		configuration.baseURL, configuration.outputDirectory, configuration.booksPerPage, // Supplies the first three config values.
		configuration.maximumRetries, configuration.startingPageNumber, configuration.pauseAfterDownload, // Supplies the remaining config values.
	) // Ends the startup log line.

	// shutdownContext is cancelled the moment the user presses Ctrl+C (SIGINT)
	// or the process receives a termination signal (SIGTERM), so any in-flight
	// HTTP request or sleep can unblock immediately instead of hanging.
	shutdownContext, stopListeningForSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM) // Wires OS signals into a cancellable context.
	defer stopListeningForSignals()                                                                                       // Ensures the signal listener is released when main returns.

	httpClient := &http.Client{Timeout: configuration.httpTimeout} // Creates the single HTTP client shared by every request in this run.

	if directoryError := os.MkdirAll(configuration.outputDirectory, 0755); directoryError != nil { // Creates the output directory if it does not already exist.
		log.Fatalf("failed to create output directory %q: %v", configuration.outputDirectory, directoryError) // Logs the fatal error and exits, since we can't save anything without this directory.
	} // Ends the directory-creation check.

	statistics := &runStatistics{} // Creates the statistics tracker used for the whole run.

	totalBooksSeenSoFar := 0 // Tracks how many books the catalog API has reported across all pages processed so far.

pageProcessingLoop: // Labels the pagination loop so a shutdown request can break out of it cleanly from a nested loop.
	for pageNumber := configuration.startingPageNumber; ; pageNumber++ { // Keeps requesting the next catalog page until the API reports no more books.
		select { // Checks whether a shutdown was requested before starting work on a new page.
		case <-shutdownContext.Done(): // Fires if the user asked the program to stop.
			log.Println("shutdown requested, stopping before requesting the next catalog page") // Logs that pagination is stopping early because of a shutdown request.
			break pageProcessingLoop                                                            // Exits the pagination loop immediately.
		default: // Falls through immediately when no shutdown has been requested.
		} // Ends the shutdown check.

		catalogPage, catalogError := fetchCatalogPage(shutdownContext, httpClient, configuration, pageNumber) // Fetches and decodes one catalog page, retrying internally on transient failures.
		if catalogError != nil {                                                                              // Checks whether the page could not be fetched even after retries.
			log.Printf("catalog page %d: giving up after retries: %v", pageNumber, catalogError) // Logs the terminal catalog-fetch error.
			break pageProcessingLoop                                                             // Stops the whole run, since without a catalog page we have no more books to process.
		} // Ends the catalog-fetch error check.

		if catalogPage.HasError { // Checks whether the API itself reported an error inside a successful HTTP response.
			log.Println("the API reported an error in its response body, stopping") // Reports the API-level error to the log.
			break pageProcessingLoop                                                // Stops pagination.
		} // Ends the API-error check.

		if len(catalogPage.Books) == 0 { // Checks whether this page came back with no books at all.
			log.Println("the API returned no more books, pagination is complete") // Reports that we have reached the end of the catalog.
			break pageProcessingLoop                                              // Stops pagination; this is the normal, successful end of the run.
		} // Ends the empty-page check.

		totalBooksSeenSoFar += len(catalogPage.Books) // Adds this page's book count to the running total across the whole run.
		log.Printf(                                   // Logs progress for this page.
			"page %d: got %d books (API-reported total=%d, seen so far=%d)", // Defines the per-page progress log template.
			pageNumber, len(catalogPage.Books), catalogPage.TotalBookCount, totalBooksSeenSoFar, // Supplies the values being logged.
		) // Ends the per-page progress log line.

		for _, book := range catalogPage.Books { // Processes every book on the current page, one at a time (no parallel downloads).
			select { // Checks whether a shutdown was requested before starting the next download.
			case <-shutdownContext.Done(): // Fires if a shutdown was requested partway through a page.
				log.Println("shutdown requested, stopping before starting the next download") // Logs the early stop.
				break pageProcessingLoop                                                      // Leaves both loops; there is nothing else running, since downloads happen one at a time.
			default: // Falls through immediately when no shutdown has been requested.
			} // Ends the shutdown check.

			downloadBookPDF(shutdownContext, httpClient, configuration, statistics, book) // Downloads this one book's PDF (with its own retries and logging) before moving on to the next book.
		} // Ends the per-book download loop.

		log.Printf("page %d finished (running totals: %s)", pageNumber, statistics.summary()) // Logs a progress snapshot after every page finishes processing.
	} // Ends the pagination loop.

	totalElapsedTime := time.Since(runStartTime)                                                     // Computes how long the whole run took, from start to finish.
	log.Printf("run complete in %s — %s", totalElapsedTime.Round(time.Second), statistics.summary()) // Logs the final summary line.

	if failedCount := statistics.booksFailed.Load(); failedCount > 0 { // Checks whether any downloads ultimately failed after all retries.
		log.Printf( // Points the user at the program's built-in resume behavior.
			"%d book(s) failed after %d retries each; re-run the program to retry them (already-completed files are skipped automatically)", // Defines the failure-summary message template.
			failedCount, configuration.maximumRetries, // Supplies the failure count and retry limit being logged.
		) // Ends the failure-summary log line.
	} // Ends the failure-summary check.
} // Ends the main function.

// ---------------------------------------------------------------------------
// Catalog fetching
// ---------------------------------------------------------------------------

// fetchCatalogPage requests and decodes a single catalog page, retrying on
// transient network or server errors using exponential backoff with jitter.
func fetchCatalogPage(shutdownContext context.Context, httpClient *http.Client, configuration runConfiguration, pageNumber int) (catalogResponse, error) { // Starts the catalog-page fetcher with built-in retries.
	var mostRecentError error // Tracks the most recent attempt's error, used in the final failure message if every attempt fails.

	for attemptNumber := 0; attemptNumber <= configuration.maximumRetries; attemptNumber++ { // Tries the request up to maximumRetries+1 times in total.
		if attemptNumber > 0 { // Skips the backoff delay before the very first attempt.
			backoffDuration := computeBackoffDelay(configuration.retryBackoffBaseDelay, attemptNumber)                                                         // Computes an exponential-with-jitter delay for this retry.
			log.Printf("catalog page %d: retry %d/%d in %s", pageNumber, attemptNumber, configuration.maximumRetries, backoffDuration.Round(time.Millisecond)) // Logs that a retry is about to happen and how long we're waiting.
			select {                                                                                                                                           // Waits for either the backoff delay to elapse or a shutdown request, whichever comes first.
			case <-time.After(backoffDuration): // Fires once the backoff delay has fully elapsed.
			case <-shutdownContext.Done(): // Fires if a shutdown was requested while we were waiting to retry.
				return catalogResponse{}, shutdownContext.Err() // Aborts immediately, propagating the cancellation reason.
			} // Ends the backoff wait.
		} // Ends the retry-delay branch.

		catalogPage, requestError := performCatalogPageRequest(shutdownContext, httpClient, configuration, pageNumber) // Performs exactly one HTTP round trip for this catalog page.
		if requestError == nil {                                                                                       // Checks whether this attempt succeeded.
			return catalogPage, nil // Returns the successfully decoded catalog page immediately.
		} // Ends the success check.

		mostRecentError = requestError                                                                                                     // Remembers this attempt's error in case every attempt ultimately fails.
		log.Printf("catalog page %d: attempt %d/%d failed: %v", pageNumber, attemptNumber+1, configuration.maximumRetries+1, requestError) // Logs the failed attempt along with its error.
	} // Ends the retry loop.

	return catalogResponse{}, fmt.Errorf("all attempts failed: %w", mostRecentError) // Returns the wrapped final error after every retry attempt has been exhausted.
} // Ends fetchCatalogPage.

// performCatalogPageRequest performs exactly one HTTP GET request for a
// single catalog page and decodes the JSON response, without any retrying
// of its own (retrying is handled by the caller, fetchCatalogPage).
func performCatalogPageRequest(shutdownContext context.Context, httpClient *http.Client, configuration runConfiguration, pageNumber int) (catalogResponse, error) { // Starts the single-attempt catalog fetcher.
	catalogURL, parseError := url.Parse(configuration.baseURL + "/api/v1/commons/catalog") // Builds the base catalog API URL.
	if parseError != nil {                                                                 // Checks whether the base URL string failed to parse.
		return catalogResponse{}, fmt.Errorf("parsing catalog URL: %w", parseError) // Returns a wrapped URL-parsing error.
	} // Ends the URL-parse check.

	queryParameters := catalogURL.Query()                                  // Reads the (currently empty) query-string parameters so we can add to them.
	queryParameters.Set("activePage", strconv.Itoa(pageNumber))            // Sets which catalog page we're requesting.
	queryParameters.Set("limit", strconv.Itoa(configuration.booksPerPage)) // Sets how many books we want returned per page.
	catalogURL.RawQuery = queryParameters.Encode()                         // Writes the encoded query parameters back onto the URL.

	httpRequest, requestBuildError := http.NewRequestWithContext(shutdownContext, http.MethodGet, catalogURL.String(), nil) // Builds a cancellable GET request for the catalog page.
	if requestBuildError != nil {                                                                                           // Checks whether the request object itself could not be constructed.
		return catalogResponse{}, fmt.Errorf("building request: %w", requestBuildError) // Returns a wrapped request-construction error.
	} // Ends the request-construction check.
	httpRequest.Header.Set("User-Agent", configuration.userAgent) // Identifies this program to the server using the configured User-Agent string.
	httpRequest.Header.Set("Accept", "application/json")          // Tells the server we expect a JSON response body.

	log.Printf("requesting catalog page %d: %s", pageNumber, catalogURL.String()) // Logs the exact catalog URL being requested, so the network activity is fully traceable in the logs.

	httpResponse, requestError := httpClient.Do(httpRequest) // Sends the catalog request over the network.
	if requestError != nil {                                 // Checks whether the network request itself failed (timeout, DNS failure, connection refused, etc).
		return catalogResponse{}, fmt.Errorf("request error: %w", requestError) // Returns a wrapped network error.
	} // Ends the request-error check.
	defer httpResponse.Body.Close() // Ensures the response body is always closed, even if decoding fails below.

	log.Printf("received HTTP %d for catalog page %d", httpResponse.StatusCode, pageNumber) // Logs the response status as soon as headers arrive, before the body is decoded.

	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 { // Checks whether the server returned a non-success (non-2xx) HTTP status code.
		return catalogResponse{}, fmt.Errorf("HTTP %d from catalog API", httpResponse.StatusCode) // Returns an error describing the unexpected status code.
	} // Ends the status-code check.

	var decodedCatalogPage catalogResponse                                                                 // Declares the variable that will hold the decoded JSON response.
	if decodeError := json.NewDecoder(httpResponse.Body).Decode(&decodedCatalogPage); decodeError != nil { // Decodes the JSON response body into decodedCatalogPage.
		return catalogResponse{}, fmt.Errorf("decoding JSON: %w", decodeError) // Returns a wrapped JSON-decoding error.
	} // Ends the JSON-decode check.

	return decodedCatalogPage, nil // Returns the successfully decoded catalog page.
} // Ends performCatalogPageRequest.

// ---------------------------------------------------------------------------
// PDF downloading
// ---------------------------------------------------------------------------

// downloadBookPDF downloads one book's PDF to disk. It skips the download if
// a valid copy already exists, retries transient failures with backoff, and
// always writes through a ".part" temporary file so a crash mid-download can
// never leave a corrupt file behind under the final name. On a successful
// download it pauses for configuration.pauseAfterDownload before returning,
// so the next book's request is spaced out from this one.
func downloadBookPDF(shutdownContext context.Context, httpClient *http.Client, configuration runConfiguration, statistics *runStatistics, book catalogBook) { // Starts the per-book download function.
	if book.BookID == "" { // Checks whether this catalog entry is missing its unique book identifier.
		log.Println("skipping a book with no bookID in the catalog response") // Reports the invalid catalog entry.
		return                                                                // Skips this entry entirely; there's nothing useful we can do with it.
	} // Ends the missing-ID check.

	if book.Links.PDF == "" { // Checks whether this book has no PDF download link at all.
		log.Printf("skip %s: no PDF link available", book.BookID) // Reports that this book has nothing to download.
		statistics.booksWithNoPDF.Add(1)                          // Records the no-PDF outcome in the run statistics.
		return                                                    // Skips this book.
	} // Ends the missing-PDF-link check.

	finalFilePath := filepath.Join(configuration.outputDirectory, book.BookID+".pdf") // Builds the path where the completed PDF will ultimately live.

	log.Printf("book %s: source URL=%s destination path=%s", book.BookID, book.Links.PDF, finalFilePath) // Logs exactly where this PDF is coming from and where it will be saved, before anything else happens.

	if existingFileInfo, statError := os.Stat(finalFilePath); statError == nil && existingFileInfo.Size() > 0 { // Checks whether a non-empty PDF already exists at that path.
		log.Printf("skip %s: already downloaded (%s) at %s", book.BookID, formatByteCountForHumans(existingFileInfo.Size()), finalFilePath) // Reports that this book was already downloaded in a previous run, including where the existing file lives.
		statistics.booksSkipped.Add(1)                                                                                                      // Records the skip in the run statistics.
		return                                                                                                                              // Leaves the existing file untouched.
	} // Ends the existing-file check.

	var mostRecentError error // Tracks the most recent attempt's error, used in the final failure log if every attempt fails.

	for attemptNumber := 0; attemptNumber <= configuration.maximumRetries; attemptNumber++ { // Tries the download up to maximumRetries+1 times in total.
		if attemptNumber > 0 { // Skips the backoff delay before the very first attempt.
			backoffDuration := computeBackoffDelay(configuration.retryBackoffBaseDelay, attemptNumber) // Computes an exponential-with-jitter delay for this retry.
			log.Printf(                                                                                // Logs that a retry is about to happen, including the reason for the previous failure.
				"%s: retry %d/%d in %s (previous error: %v)", // Defines the retry-log message template.
				book.BookID, attemptNumber, configuration.maximumRetries, backoffDuration.Round(time.Millisecond), mostRecentError, // Supplies the values being logged.
			) // Ends the retry log line.
			select { // Waits for either the backoff delay to elapse or a shutdown request, whichever comes first.
			case <-time.After(backoffDuration): // Fires once the backoff delay has fully elapsed.
			case <-shutdownContext.Done(): // Fires if a shutdown was requested while we were waiting to retry.
				log.Printf("%s: aborting retries, shutdown in progress", book.BookID) // Logs that this book's retries are being abandoned due to shutdown.
				statistics.booksFailed.Add(1)                                         // Counts the aborted download as a failure for the final summary.
				return                                                                // Stops retrying this book and returns immediately.
			} // Ends the backoff wait.
		} // Ends the retry-delay branch.

		log.Printf("attempt %d/%d for %s: downloading %s -> %s", attemptNumber+1, configuration.maximumRetries+1, book.BookID, book.Links.PDF, finalFilePath) // Logs which attempt this is and the exact source-to-destination mapping being tried.

		bytesWrittenThisAttempt, downloadError := performSingleDownloadAttempt(shutdownContext, httpClient, configuration, book, finalFilePath) // Performs exactly one full download attempt for this book.
		if downloadError == nil {                                                                                                               // Checks whether this attempt succeeded.
			log.Printf("done %s: wrote %s to %s (from %s)", book.BookID, formatByteCountForHumans(bytesWrittenThisAttempt), finalFilePath, book.Links.PDF) // Logs the successful download, how many bytes were written, and both the source URL and destination path.
			statistics.booksDownloaded.Add(1)                                                                                                              // Records the success in the run statistics.
			statistics.totalBytesWritten.Add(bytesWrittenThisAttempt)                                                                                      // Adds the bytes written by this download to the running total.

			log.Printf("sleeping %s before the next download", configuration.pauseAfterDownload) // Logs the pause so the run doesn't look stalled while it waits.
			select {                                                                             // Waits for either the pause to elapse or a shutdown request, whichever comes first.
			case <-time.After(configuration.pauseAfterDownload): // Fires once the post-download pause has fully elapsed.
			case <-shutdownContext.Done(): // Fires if a shutdown was requested during the pause; we still return normally below.
			} // Ends the post-download pause.

			return // This book is finished; no further retries are needed.
		} // Ends the success check.

		mostRecentError = downloadError // Remembers this attempt's error in case every attempt ultimately fails.
	} // Ends the retry loop.

	log.Printf("fail %s: giving up after %d attempts: %v", book.BookID, configuration.maximumRetries+1, mostRecentError) // Logs the terminal failure for this book, including the last error seen.
	statistics.booksFailed.Add(1)                                                                                        // Records the failure in the run statistics.
} // Ends downloadBookPDF.

// performSingleDownloadAttempt performs exactly one HTTP GET request plus a
// file write for a book's PDF, returning the number of bytes written on
// success. While streaming the response to disk it also computes a SHA-256
// checksum of the bytes as they arrive; after the file is renamed into
// place, it re-reads the file from disk and computes the checksum again to
// confirm the two match, guarding against corruption introduced while
// writing or renaming the file. It does not retry on its own; retrying is
// handled by the caller, downloadBookPDF.
func performSingleDownloadAttempt(shutdownContext context.Context, httpClient *http.Client, configuration runConfiguration, book catalogBook, finalFilePath string) (int64, error) { // Starts the single-attempt downloader.
	log.Printf("requesting %s", book.Links.PDF) // Logs the exact URL this attempt is about to request, so the network activity is fully traceable in the logs.

	httpRequest, requestBuildError := http.NewRequestWithContext(shutdownContext, http.MethodGet, book.Links.PDF, nil) // Builds a cancellable GET request for the book's PDF.
	if requestBuildError != nil {                                                                                      // Checks whether the request object itself could not be constructed.
		return 0, fmt.Errorf("building request: %w", requestBuildError) // Returns a wrapped request-construction error.
	} // Ends the request-construction check.
	httpRequest.Header.Set("User-Agent", configuration.userAgent) // Identifies this program to the server using the configured User-Agent string.

	httpResponse, requestError := httpClient.Do(httpRequest) // Sends the PDF download request over the network.
	if requestError != nil {                                 // Checks whether the network request itself failed.
		return 0, fmt.Errorf("request error: %w", requestError) // Returns a wrapped network error.
	} // Ends the request-error check.
	defer httpResponse.Body.Close() // Ensures the response body is always closed, even if the copy below fails.

	log.Printf("received HTTP %d from %s (content-length=%s)", httpResponse.StatusCode, book.Links.PDF, httpResponse.Header.Get("Content-Length")) // Logs the response status and advertised size as soon as headers arrive, before any bytes are copied to disk.

	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 { // Checks whether the server returned a non-success (non-2xx) HTTP status code.
		return 0, fmt.Errorf("HTTP %d", httpResponse.StatusCode) // Returns an error describing the unexpected status code.
	} // Ends the status-code check.

	temporaryFilePath := finalFilePath + ".part" // Builds the temporary-file path used while the download is still in progress.

	log.Printf("writing to temporary file %s (will become %s on success)", temporaryFilePath, finalFilePath) // Logs both the temporary path being written to now and the final path it will be renamed to once complete.

	temporaryFile, fileCreateError := os.Create(temporaryFilePath) // Creates (or truncates) the temporary file that the PDF will be streamed into.
	if fileCreateError != nil {                                    // Checks whether the temporary file could not be created.
		return 0, fmt.Errorf("creating temp file: %w", fileCreateError) // Returns a wrapped filesystem error.
	} // Ends the file-creation check.

	downloadChecksumHasher := sha256.New()                                          // Creates a SHA-256 hasher that will be fed every byte as it's streamed to disk, so we know the checksum of exactly what we downloaded.
	temporaryFileAndHasher := io.MultiWriter(temporaryFile, downloadChecksumHasher) // Combines the temp file and the hasher into one writer, so a single io.Copy writes to both at once.
	bytesWritten, copyError := io.Copy(temporaryFileAndHasher, httpResponse.Body)   // Streams the PDF response body into the temporary file (and the hasher), tracking how many bytes were written.
	fileCloseError := temporaryFile.Close()                                         // Closes the temporary file regardless of whether the copy above succeeded.

	if copyError != nil { // Checks whether the copy failed partway through (e.g. the connection dropped mid-download).
		os.Remove(temporaryFilePath)                        // Removes the incomplete temporary file so it can never be mistaken for a finished download.
		return 0, fmt.Errorf("writing file: %w", copyError) // Returns a wrapped copy error.
	} // Ends the copy-error check.

	if fileCloseError != nil { // Checks whether closing the file failed (for example, if the disk became full while flushing buffers).
		os.Remove(temporaryFilePath)                             // Removes the potentially corrupt temporary file.
		return 0, fmt.Errorf("closing file: %w", fileCloseError) // Returns a wrapped close error.
	} // Ends the close-error check.

	downloadedChecksum := hex.EncodeToString(downloadChecksumHasher.Sum(nil))                                 // Renders the SHA-256 checksum computed while streaming as a hex string, e.g. "9f86d0...".
	log.Printf("computed SHA-256 checksum of downloaded bytes for %s: %s", finalFilePath, downloadedChecksum) // Logs the checksum computed from the network stream, before anything is verified against disk.

	log.Printf("renaming %s to %s", temporaryFilePath, finalFilePath) // Logs the final rename step that promotes the temp file to its permanent name and location.

	if renameError := os.Rename(temporaryFilePath, finalFilePath); renameError != nil { // Atomically promotes the temporary file to its final filename, now that it's fully and correctly written.
		os.Remove(temporaryFilePath)                           // Cleans up the temporary file if the rename itself failed.
		return 0, fmt.Errorf("renaming file: %w", renameError) // Returns a wrapped rename error.
	} // Ends the rename check.

	log.Printf("verifying checksum of %s against downloaded checksum %s", finalFilePath, downloadedChecksum) // Logs that we're about to re-read the file from disk to confirm it matches what we downloaded.

	onDiskChecksum, checksumError := computeSHA256ChecksumOfFile(finalFilePath) // Re-reads the final file from disk and computes its checksum independently of the in-memory one above.
	if checksumError != nil {                                                   // Checks whether the file could not be read back for verification.
		os.Remove(finalFilePath)                                                               // Removes the file, since we can't confirm it's intact.
		return 0, fmt.Errorf("reading file back for checksum verification: %w", checksumError) // Returns a wrapped checksum-verification error.
	} // Ends the checksum-read check.

	if onDiskChecksum != downloadedChecksum { // Checks whether the checksum of the bytes on disk matches the checksum computed while downloading.
		os.Remove(finalFilePath)                                                                                                      // Removes the corrupted file so it isn't mistaken for a good download on the next run.
		return 0, fmt.Errorf("checksum mismatch for %s: downloaded=%s on-disk=%s", finalFilePath, downloadedChecksum, onDiskChecksum) // Returns a descriptive checksum-mismatch error.
	} // Ends the checksum-comparison check.

	log.Printf("checksum verified OK for %s: %s", finalFilePath, onDiskChecksum) // Logs that the file on disk was confirmed to exactly match what was downloaded.

	return bytesWritten, nil // Returns the number of bytes successfully written to the final file.
} // Ends performSingleDownloadAttempt.

// computeSHA256ChecksumOfFile opens the file at the given path and streams
// its full contents through a SHA-256 hasher, returning the resulting
// checksum as a lowercase hex string. It is used after a download completes
// to confirm the bytes actually sitting on disk match the bytes that were
// streamed in from the network.
func computeSHA256ChecksumOfFile(filePath string) (string, error) { // Starts the file-checksum helper.
	openedFile, openError := os.Open(filePath) // Opens the file for reading; this does not load it all into memory at once.
	if openError != nil {                      // Checks whether the file could not be opened.
		return "", fmt.Errorf("opening file: %w", openError) // Returns a wrapped open error.
	} // Ends the open-error check.
	defer openedFile.Close() // Ensures the file handle is always closed once this function returns.

	checksumHasher := sha256.New()                                             // Creates a fresh SHA-256 hasher for this file.
	if _, copyError := io.Copy(checksumHasher, openedFile); copyError != nil { // Streams the entire file through the hasher in fixed-size chunks, without loading it all into memory.
		return "", fmt.Errorf("hashing file: %w", copyError) // Returns a wrapped hashing error.
	} // Ends the copy-error check.

	return hex.EncodeToString(checksumHasher.Sum(nil)), nil // Returns the final checksum as a lowercase hex string.
} // Ends computeSHA256ChecksumOfFile.

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// computeBackoffDelay computes an exponential backoff delay
// (baseDelay * 2^(attemptNumber-1)) with up to ~30% random jitter added, so
// that repeated retries don't all hammer the server at exactly the same
// instant as one another.
func computeBackoffDelay(baseDelay time.Duration, attemptNumber int) time.Duration { // Starts the backoff-delay calculation helper.
	backoffMultiplier := 1 << (attemptNumber - 1)                     // Doubles the delay for each additional attempt: 1x, 2x, 4x, 8x, and so on.
	delayBeforeJitter := baseDelay * time.Duration(backoffMultiplier) // Scales the base delay by the multiplier computed above.

	randomJitter := time.Duration(rand.Int63n(int64(delayBeforeJitter) / 3)) // Adds up to roughly 30% random jitter on top of the base delay.
	return delayBeforeJitter + randomJitter                                  // Returns the final, jittered backoff delay.
} // Ends computeBackoffDelay.
