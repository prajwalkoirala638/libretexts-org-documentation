package main // Defines this file as an executable Go program.

import ( // Starts the list of packages required by the program.
	"encoding/json" // Provides JSON decoding for the LibreTexts API response.
	"fmt"           // Provides functions for printing progress and errors.
	"io"            // Provides the io.Copy function for streaming PDF data.
	"net/http"      // Provides HTTP client functionality for API requests and PDF downloads.
	"net/url"       // Provides URL parsing and query-string construction.
	"os"            // Provides filesystem operations such as creating and checking files.
	"path/filepath" // Provides portable filesystem path handling.
	"strconv"       // Converts numbers to strings for API query parameters.
	"time"          // Provides HTTP request timeout configuration.
) // Ends the package import list.

const ( // Starts the program configuration constants.
	baseURL   = "https://commons.libretexts.org" // Defines the LibreTexts Commons base URL.
	outputDir = "PDFs"                           // Defines the folder where PDFs will be saved.
	pageSize  = 100                              // Requests up to 100 books from the API on each page.
) // Ends the configuration constants.

type Catalog struct { // Defines the structure of the catalog API response.
	Err      bool   `json:"err"`      // Stores the API error flag.
	NumTotal int    `json:"numTotal"` // Stores the total number of books reported by the API.
	Books    []Book `json:"books"`    // Stores the books returned on the current API page.
	Seed     int    `json:"seed"`     // Stores the API seed, which is not used for pagination.
} // Ends the Catalog structure.

type Book struct { // Defines the book information needed by this program.
	BookID string   `json:"bookID"` // Stores the unique ID of the LibreTexts book.
	Links  struct { // Defines the links returned for a book.
		PDF string `json:"pdf"` // Stores the URL of the book's PDF.
	} `json:"links"` // Maps the JSON links object to the Links structure.
} // Ends the Book structure.

var client = &http.Client{ // Creates one reusable HTTP client for all requests.
	Timeout: 2 * time.Minute, // Gives each HTTP request a maximum duration of two minutes.
} // Ends the HTTP client configuration.

func main() { // Defines the main entry point of the program.
	err := os.MkdirAll(outputDir, 0755) // Creates the PDFs directory if it does not already exist.
	if err != nil {                     // Checks whether creating the directory failed.
		fmt.Println("Failed to create PDFs directory:", err) // Prints the directory creation error.
		return                                               // Stops the program because PDFs cannot be saved.
	} // Ends the directory creation error check.

	var books []Book // Creates a slice that will contain all books discovered from the API.

	for page := 1; ; page++ { // Requests catalog pages sequentially until the API returns no books.
		u, err := url.Parse(baseURL + "/api/v1/commons/catalog") // Creates the LibreTexts catalog API URL.
		if err != nil {                                          // Checks whether the URL could not be parsed.
			fmt.Println("URL error:", err) // Prints the URL parsing error.
			return                         // Stops the program.
		} // Ends the URL error check.

		q := u.Query()                          // Gets the URL query parameters.
		q.Set("activePage", strconv.Itoa(page)) // Sets the current API page number.
		q.Set("limit", strconv.Itoa(pageSize))  // Sets the number of books requested per page.
		u.RawQuery = q.Encode()                 // Encodes the query parameters into the final URL.

		fmt.Printf("Getting catalog page %d...\n", page) // Displays which catalog page is being requested.

		resp, err := client.Get(u.String()) // Sends the GET request to the LibreTexts catalog API.
		if err != nil {                     // Checks whether the HTTP request failed.
			fmt.Println("Catalog error:", err) // Prints the API request error.
			return                             // Stops immediately if the API request itself fails.
		} // Ends the HTTP request error check.

		var catalog Catalog // Creates a variable for the API response.

		err = json.NewDecoder(resp.Body).Decode(&catalog) // Decodes the API JSON response.
		resp.Body.Close()                                 // Closes the API response body.

		if err != nil { // Checks whether the JSON could not be decoded.
			fmt.Println("JSON error:", err) // Prints the JSON decoding error.
			return                          // Stops because the API response could not be understood.
		} // Ends the JSON decoding error check.

		if catalog.Err { // Checks whether the API explicitly reported an error.
			fmt.Println("API returned an error. Stopping.") // Reports the API error condition.
			return                                          // Stops immediately when the API error flag is true.
		} // Ends the API error check.

		if len(catalog.Books) == 0 { // Checks whether the API returned an empty books array.
			fmt.Println("API returned no more books. Stopping.") // Reports that there are no more books.
			break                                                // Stops pagination immediately.
		} // Ends the empty-books check.

		books = append(books, catalog.Books...) // Adds all books from this page to the complete book list.

		fmt.Printf("Found %d/%d books\n", len(books), catalog.NumTotal) // Displays catalog progress.
	} // Ends the catalog pagination loop.

	fmt.Printf("Found %d books with data. Starting PDF downloads...\n", len(books)) // Announces that PDF downloads are starting.

	for _, book := range books { // Processes each book sequentially.
		downloadPDF(book) // Attempts to download only the PDF for the current book.
	} // Ends the book processing loop.

	fmt.Println("Finished.") // Reports that all discovered books have been processed.
} // Ends the main function.

func downloadPDF(book Book) { // Downloads the PDF belonging to one book.
	if book.BookID == "" { // Checks whether the book has a valid book ID.
		return // Ignores books without an ID.
	} // Ends the book ID check.

	if book.Links.PDF == "" { // Checks whether the book has a PDF URL.
		fmt.Println("Skipping:", book.BookID, "— no PDF") // Reports that this book has no PDF.
		return                                            // Ignores the book because there is no PDF to download.
	} // Ends the PDF URL check.

	filename := filepath.Join(outputDir, book.BookID+".pdf") // Creates the destination filename using the book ID.

	if _, err := os.Stat(filename); err == nil { // Checks whether the PDF already exists.
		fmt.Println("Skipping:", book.BookID, "— already downloaded") // Reports that the PDF already exists.
		return                                                        // Skips the existing PDF.
	} // Ends the existing-file check.

	fmt.Println("Downloading:", book.BookID) // Displays the book currently being downloaded.

	resp, err := client.Get(book.Links.PDF) // Sends an HTTP GET request to the PDF URL.
	if err != nil {                         // Checks whether the PDF request failed.
		fmt.Println("Failed:", book.BookID, err) // Prints the download error.
		return                                   // Moves on to the next book.
	} // Ends the PDF request error check.

	defer resp.Body.Close() // Ensures the PDF response body is closed when this function finishes.

	if resp.StatusCode < 200 || resp.StatusCode >= 300 { // Checks whether the server returned an unsuccessful HTTP status.
		fmt.Printf("Failed: %s HTTP %d\n", book.BookID, resp.StatusCode) // Prints the HTTP error.
		return                                                           // Moves on to the next book.
	} // Ends the HTTP status check.

	temp := filename + ".part" // Creates a temporary filename for the in-progress PDF.

	file, err := os.Create(temp) // Creates the temporary PDF file.
	if err != nil {              // Checks whether the temporary file could not be created.
		fmt.Println("Failed to create:", temp, err) // Prints the filesystem error.
		return                                      // Moves on to the next book.
	} // Ends the temporary file creation check.

	_, err = io.Copy(file, resp.Body) // Streams the complete PDF response into the temporary file.
	closeErr := file.Close()          // Closes the temporary file after writing finishes.

	if err != nil { // Checks whether downloading the PDF failed.
		fmt.Println("Download failed:", book.BookID, err) // Prints the download error.
		os.Remove(temp)                                   // Deletes the incomplete temporary file.
		return                                            // Moves on to the next book.
	} // Ends the download error check.

	if closeErr != nil { // Checks whether closing the temporary file failed.
		fmt.Println("File close failed:", book.BookID, closeErr) // Prints the file closing error.
		os.Remove(temp)                                          // Deletes the potentially incomplete file.
		return                                                   // Moves on to the next book.
	} // Ends the file closing error check.

	err = os.Rename(temp, filename) // Renames the completed temporary file to its final PDF filename.
	if err != nil {                 // Checks whether the rename failed.
		fmt.Println("Rename failed:", book.BookID, err) // Prints the rename error.
		os.Remove(temp)                                 // Removes the temporary file.
		return                                          // Moves on to the next book.
	} // Ends the rename error check.

	fmt.Println("Downloaded:", filename) // Reports that the PDF was successfully downloaded.
} // Ends the downloadPDF function.
