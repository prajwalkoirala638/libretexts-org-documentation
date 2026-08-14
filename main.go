package main // Defines this file as an executable Go program.

import ( // Starts the list of packages used by the program.
	"encoding/json" // Decodes JSON responses from the LibreTexts API.
	"fmt"           // Prints messages and progress to the terminal.
	"io"            // Streams downloaded PDF data into files.
	"net/http"      // Makes HTTP requests to the API and PDF URLs.
	"net/url"       // Builds URLs with query parameters.
	"os"            // Handles files and directories.
	"path/filepath" // Builds filesystem paths safely.
	"strconv"       // Converts page numbers and limits to strings.
	"time"          // Provides the HTTP request timeout.
) // Ends the import list.

const ( // Starts the program configuration.
	baseURL   = "https://commons.libretexts.org" // Defines the LibreTexts Commons base URL.
	outputDir = "PDFs"                           // Defines the directory where PDFs will be stored.
	pageSize  = 100                              // Requests 100 books per API page.
) // Ends the configuration.

type Catalog struct { // Defines the structure of the catalog API response.
	Err      bool   `json:"err"`      // Stores the API error flag.
	NumTotal int    `json:"numTotal"` // Stores the total number of books.
	Books    []Book `json:"books"`    // Stores the books returned by the API.
	Seed     int    `json:"seed"`     // Stores the API seed, although it is not used.
} // Ends the Catalog structure.

type Book struct { // Defines the book information we need.
	BookID string   `json:"bookID"` // Stores the unique LibreTexts book ID.
	Links  struct { // Defines the book's available links.
		PDF string `json:"pdf"` // Stores the PDF download URL.
	} `json:"links"` // Maps the JSON links object.
} // Ends the Book structure.

var client = &http.Client{ // Creates a reusable HTTP client.
	Timeout: 2 * time.Minute, // Allows each request to run for up to two minutes.
} // Ends the HTTP client configuration.

func main() { // Starts the program.
	err := os.MkdirAll(outputDir, 0755) // Creates the PDFs directory if it does not exist.
	if err != nil {                     // Checks whether directory creation failed.
		fmt.Println("Failed to create PDFs directory:", err) // Prints the error.
		return                                               // Stops the program.
	} // Ends the directory error check.

	for page := 1; ; page++ { // Continues requesting pages until the API returns an empty books array.
		u, err := url.Parse(baseURL + "/api/v1/commons/catalog") // Creates the catalog API URL.
		if err != nil {                                          // Checks whether the URL could not be parsed.
			fmt.Println("URL error:", err) // Prints the URL error.
			return                         // Stops the program.
		} // Ends the URL error check.

		q := u.Query()                          // Gets the URL query parameters.
		q.Set("activePage", strconv.Itoa(page)) // Sets the current catalog page.
		q.Set("limit", strconv.Itoa(pageSize))  // Sets the number of books requested.
		u.RawQuery = q.Encode()                 // Adds the query parameters to the URL.

		fmt.Printf("\nGetting catalog page %d...\n", page) // Shows which page is being requested.

		resp, err := client.Get(u.String()) // Requests the catalog page.
		if err != nil {                     // Checks whether the API request failed.
			fmt.Println("Catalog error:", err) // Prints the API error.
			return                             // Stops the program on an API error.
		} // Ends the request error check.

		var catalog Catalog // Creates a variable to hold the API response.

		err = json.NewDecoder(resp.Body).Decode(&catalog) // Decodes the API response into the Catalog structure.
		resp.Body.Close()                                 // Closes the API response body.

		if err != nil { // Checks whether JSON decoding failed.
			fmt.Println("JSON error:", err) // Prints the JSON error.
			return                          // Stops the program.
		} // Ends the JSON error check.

		if catalog.Err { // Checks whether the API explicitly reported an error.
			fmt.Println("API returned an error. Stopping.") // Reports the API error.
			return                                          // Stops the program.
		} // Ends the API error check.

		if len(catalog.Books) == 0 { // Checks whether the API returned an empty books array.
			fmt.Println("API returned no more books. Stopping.") // Reports that there are no more books.
			break                                                // Stops pagination.
		} // Ends the empty-books check.

		fmt.Printf("Found %d books on page %d. Downloading PDFs...\n", len(catalog.Books), page) // Reports the page size.

		for _, book := range catalog.Books { // Processes every book on the current page.
			downloadPDF(book) // Downloads the PDF or skips it if it already exists.
		} // Ends the book loop.

		fmt.Printf("Finished page %d.\n", page) // Reports that the current page has been processed.
	} // Ends the pagination loop.

	fmt.Println("All pages processed.") // Reports that the entire catalog has been processed.
} // Ends the main function.

func downloadPDF(book Book) { // Downloads the PDF for one book.
	if book.BookID == "" { // Checks whether the book has a valid ID.
		fmt.Println("Skipping book with no bookID.") // Reports the invalid book.
		return                                       // Skips the book.
	} // Ends the book ID check.

	if book.Links.PDF == "" { // Checks whether a PDF URL exists.
		fmt.Println("Skipping:", book.BookID, "— no PDF available.") // Reports that no PDF exists.
		return                                                       // Skips this book.
	} // Ends the PDF URL check.

	filename := filepath.Join(outputDir, book.BookID+".pdf") // Creates the final PDF filename.

	if _, err := os.Stat(filename); err == nil { // Checks whether the PDF already exists.
		fmt.Println("Skipping:", book.BookID, "— already downloaded.") // Reports that the PDF is already downloaded.
		return                                                         // Does not download the existing PDF.
	} // Ends the existing-file check.

	fmt.Println("Downloading:", book.BookID) // Reports that the PDF will be downloaded.

	tempFilename := filename + ".part" // Creates a temporary filename for the incomplete download.

	resp, err := client.Get(book.Links.PDF) // Requests the PDF from LibreTexts.
	if err != nil {                         // Checks whether the PDF request failed.
		fmt.Println("Download failed:", book.BookID, err) // Prints the download error.
		return                                            // Moves to the next book.
	} // Ends the request error check.

	defer resp.Body.Close() // Ensures the PDF response body is closed.

	if resp.StatusCode < 200 || resp.StatusCode >= 300 { // Checks whether the server returned an HTTP error.
		fmt.Printf("Download failed: %s — HTTP %d\n", book.BookID, resp.StatusCode) // Prints the HTTP error.
		return                                                                      // Moves to the next book.
	} // Ends the HTTP status check.

	file, err := os.Create(tempFilename) // Creates the temporary PDF file.
	if err != nil {                      // Checks whether the temporary file could not be created.
		fmt.Println("File creation failed:", book.BookID, err) // Prints the filesystem error.
		return                                                 // Moves to the next book.
	} // Ends the file creation check.

	_, err = io.Copy(file, resp.Body) // Streams the PDF into the temporary file.
	closeErr := file.Close()          // Closes the temporary file.

	if err != nil { // Checks whether the download failed while writing.
		fmt.Println("Download failed:", book.BookID, err) // Prints the download error.
		os.Remove(tempFilename)                           // Removes the incomplete file.
		return                                            // Moves to the next book.
	} // Ends the download error check.

	if closeErr != nil { // Checks whether closing the file failed.
		fmt.Println("File close failed:", book.BookID, closeErr) // Prints the close error.
		os.Remove(tempFilename)                                  // Removes the potentially incomplete file.
		return                                                   // Moves to the next book.
	} // Ends the close error check.

	err = os.Rename(tempFilename, filename) // Renames the completed temporary file to the final PDF filename.
	if err != nil {                         // Checks whether renaming failed.
		fmt.Println("Rename failed:", book.BookID, err) // Prints the rename error.
		os.Remove(tempFilename)                         // Removes the temporary file.
		return                                          // Moves to the next book.
	} // Ends the rename error check.

	fmt.Println("Downloaded:", filename) // Reports successful PDF download.
} // Ends the downloadPDF function.
