package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"strings"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"encoding/json"
	"crypto/rand"
	"io"
	"time"
	"net/url"
)




const maxGoroutines = 10



var archiveMutex sync.Mutex
var tokensMutex sync.Mutex




var allowedOrigins = []string{
    "https://frame-archive.online",
    "https://pos.snapscan.io",
}


var weightsPool = sync.Pool{
	New: func() interface{} {
		return make([]int, 0, 10)
	},
}


var duplicateMutex sync.Mutex

var duplicateMap = make(map[string]time.Time)






type SnapScanData struct {
	ID                   int    `json:"id"`
	Status               string `json:"status"`
	TotalAmount          int    `json:"totalAmount"`
	TipAmount            int    `json:"tipAmount"`
	FeeAmount            int    `json:"feeAmount"`
	SettleAmount         int    `json:"settleAmount"`
	RequiredAmount       int    `json:"requiredAmount"`
	Date                 string `json:"date"`
	SnapCode             string `json:"snapCode"`
	SnapCodeReference    string `json:"snapCodeReference"`
	UserReference        string `json:"userReference"`
	MerchantReference    string `json:"merchantReference"`
	StatementReference   string `json:"statementReference,omitempty"`
	AuthCode             string `json:"authCode"`
	DeliveryAddress      string `json:"deliveryAddress,omitempty"`
	DeviceSerialNumber   string `json:"deviceSerialNumber,omitempty"`
	IsVoucher            *bool  `json:"isVoucher,omitempty"`
	IsVoucherRedemption  bool   `json:"isVoucherRedemption"`
	PaymentType          string `json:"paymentType"`
	TransactionType      string `json:"transactionType"`
	Extra                struct {
		Amount             string `json:"amount"`
		Snaptokens                 string `json:"id"`
		SnapID        string `json:"customValue"`
		MerchantID         string `json:"merchantId"`
		TransactionType    string `json:"transactionType"`
		Checksum           string `json:"checksum"`
		Strict             bool   `json:"strict"`
		RedirectUrl        string `json:"redirectUrl"`
		SuccessRedirectUrl string `json:"successRedirectUrl"`
		FailRedirectUrl    string `json:"failRedirectUrl"`
		WeGotOrderID       string `json:"wegotOrderId,omitempty"`
		Plugin             string `json:"plugin,omitempty"`
		PluginVersion      string `json:"plugin_version,omitempty"`
	} `json:"extra"`
}






func generateRandomClientID() (string, error) {
    const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
    const idLength = 12

    randomBytes := make([]byte, idLength)
    _, err := rand.Read(randomBytes)
    if err != nil {
        return "", err
    }

    for i := 0; i < idLength; i++ {
        randomBytes[i] = charset[int(randomBytes[i])%len(charset)]
    }

    return string(randomBytes), nil
}








func NewClientHandler(w http.ResponseWriter, r *http.Request, tokenDB *sql.DB) {
    tokensMutex.Lock()
    defer tokensMutex.Unlock()

    clientID, err := generateRandomClientID()
    if err != nil {
        http.Error(w, "Failed to generate client ID", http.StatusInternalServerError)
        return
    }

    for {
        var count int
        err := tokenDB.QueryRow("SELECT COUNT(*) FROM tokens WHERE paymentID = ?", clientID).Scan(&count)
        if err != nil {
            http.Error(w, "Error checking client ID uniqueness", http.StatusInternalServerError)
            return
        }

        if count == 0 {
            break
        }

        clientID, err = generateRandomClientID()
        if err != nil {
            http.Error(w, "Failed to generate client ID", http.StatusInternalServerError)
            return
        }
    }

    _, err = tokenDB.Exec("INSERT INTO tokens (customValue, paymentID) VALUES (?, ?)", 0, clientID)
    if err != nil {
        http.Error(w, "Failed to save client ID to the database", http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "text/plain")
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(clientID))
}










func CollectedHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
        return
    }
    err := r.ParseForm()
    if err != nil {
        http.Error(w, "Failed to parse form data", http.StatusBadRequest)
        return
    }

    paymentID := r.FormValue("paymentID")
    if paymentID == "" {
        http.Error(w, "Missing paymentID in the request", http.StatusBadRequest)
        return
    }

    tokensMutex.Lock()
    defer tokensMutex.Unlock()

    db, err := sql.Open("sqlite3", "token.db")
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer db.Close()

    _, err = db.Exec("UPDATE tokens SET customValue = ? WHERE paymentID = ?", 0, paymentID)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    w.WriteHeader(http.StatusOK)
    w.Write([]byte("customValue updated to 0"))
}









func CollectionHandler(w http.ResponseWriter, r *http.Request) {
    body, err := io.ReadAll(r.Body)
    if err != nil {
        http.Error(w, "Failed to read request body", http.StatusBadRequest)
        return
    }
    defer r.Body.Close()

    paymentID := string(body)

    tokensMutex.Lock()
    defer tokensMutex.Unlock()

    db, err := sql.Open("sqlite3", "token.db")
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer db.Close()

    var customValue sql.NullInt64
    err = db.QueryRow("SELECT customValue FROM tokens WHERE paymentID = ?", paymentID).Scan(&customValue)
    if err != nil {
        if err == sql.ErrNoRows {
            customValue = sql.NullInt64{Int64: 0, Valid: true}
        } else {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
    }

    if !customValue.Valid {
        customValue.Int64 = 0
        customValue.Valid = true
    }

    fmt.Fprintf(w, "%d", customValue.Int64)
}










func SnapScanWebhookHandler(w http.ResponseWriter, r *http.Request) {

	if r.Method != http.MethodPost {
		http.Error(w, "Invalid HTTP method", http.StatusMethodNotAllowed)
		return
	}

	var snapScanData SnapScanData

	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&snapScanData); err != nil {
			http.Error(w, fmt.Sprintf("Error decoding JSON: %v", err), http.StatusBadRequest)
			return
		}
	} else {
		payloadValue := r.FormValue("payload")
		decodedBody, err := url.QueryUnescape(payloadValue)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error decoding URL-encoded body: %v", err), http.StatusBadRequest)
			return
		}

		if err := json.NewDecoder(strings.NewReader(decodedBody)).Decode(&snapScanData); err != nil {
			http.Error(w, fmt.Sprintf("Error decoding JSON from URL-encoded body: %v", err), http.StatusBadRequest)
			return
		}
	}


    identifier := getIdentifier(snapScanData)
    if isDuplicate(identifier) {
        log.Printf("Duplicate request detected. Skipping processing.\n")
        http.Error(w, "Duplicate request", http.StatusConflict)
        return
    }

snaptokensInt, err := strconv.Atoi(snapScanData.Extra.Snaptokens)
if err != nil {
    http.Error(w, fmt.Sprintf("Failed to convert Snaptokens to integer: %v", err), http.StatusInternalServerError)
    return
}

    db, err := sql.Open("sqlite3", "token.db")
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer db.Close()

    if err := db.Ping(); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    tokensMutex.Lock()
    defer tokensMutex.Unlock()

    var existingcustomValue int
    err = db.QueryRow("SELECT customValue FROM tokens WHERE paymentID = ?", snapScanData.Extra.SnapID).Scan(&existingcustomValue)

if err == sql.ErrNoRows {
    _, err := db.Exec("INSERT INTO tokens (paymentID, customValue) VALUES (?, ?)", snapScanData.Extra.SnapID, 0)
    if err != nil {
        http.Error(w, fmt.Sprintf("Error inserting new entry: %v", err), http.StatusInternalServerError)
        return
    }
    existingcustomValue = 0
} else if err != nil {
    http.Error(w, fmt.Sprintf("Error querying database: %v", err), http.StatusInternalServerError)
    return
}

    newCustomValue := existingcustomValue + snaptokensInt
    _, err = db.Exec("UPDATE tokens SET customValue = ? WHERE paymentID = ?", newCustomValue, snapScanData.Extra.SnapID)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    responseText := fmt.Sprintf("Updated customValue for paymentID %s: %d", snapScanData.Extra.SnapID, newCustomValue)

    w.Header().Set("Content-Type", "text/plain")
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(responseText))
}











func getIdentifier(data SnapScanData) string {
        return fmt.Sprintf("%s|%s|%s", data.Date, data.Extra.Snaptokens, data.Extra.SnapID)
    }












func isDuplicate(identifier string) bool {
    duplicateMutex.Lock()
    defer duplicateMutex.Unlock()

	cleanupInterval := 1 * time.Hour

	for key, timestamp := range duplicateMap {
		if time.Since(timestamp) > cleanupInterval {
			delete(duplicateMap, key)
		}
	}

    if timestamp, exists := duplicateMap[identifier]; exists {
        if time.Since(timestamp) > 10*time.Minute {
            delete(duplicateMap, identifier)
            return false
        }
        return true
    }

    duplicateMap[identifier] = time.Now()
    return false
}











func waveSequenceCohesion(weights []int, wvsqnceInt []int) bool {
	for i := 0; i < len(wvsqnceInt); i++ {
		if abs(wvsqnceInt[i]-weights[i]) >= 54 {
			return false
		}
	}
	return true
}


func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}










func loggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        log.Printf("Received request: %s %s", r.Method, r.URL.Path)
        next.ServeHTTP(w, r)
    })
}







func recoveryMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if err := recover(); err != nil {
                log.Printf("Panic occurred: %v", err)
                http.Error(w, "Internal Server Error", http.StatusInternalServerError)
            }
        }()
        next.ServeHTTP(w, r)
    })
}









func parseWeights(str string) []int {
	weights := weightsPool.Get().([]int)
	defer weightsPool.Put(weights)

	weights = weights[:0]
	weightStrs := strings.Split(str, ",")
	for _, weightStr := range weightStrs {
		weight, err := strconv.Atoi(weightStr)
		if err == nil {
			weights = append(weights, weight)
		}
	}
	return weights
}







func applyMiddleware(handler http.Handler, middlewares ...mux.MiddlewareFunc) http.Handler {
    for _, mw := range middlewares {
        handler = mw(handler)
    }
    return handler
}







func handleFrameRequest(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	wvsqnce := r.FormValue("wavesequence")
	if wvsqnce == "" {
		http.Error(w, "Missing key parameter", http.StatusBadRequest)
		return
	}

        wvsqnceInt := parseWeights(wvsqnce)

        archiveMutex.Lock()
        defer archiveMutex.Unlock()

	rows, err := db.Query("SELECT links, weights FROM archive")
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var link, weightsStr string
		if err := rows.Scan(&link, &weightsStr); err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		weights := parseWeights(weightsStr)

		if waveSequenceCohesion(weights, wvsqnceInt) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, link)
			return
		}
	}

	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintln(w, "No match found")
}







func handleArchiveRequest(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()
	wvsqnce := r.FormValue("wavesequence")
	link := r.FormValue("link")
	if wvsqnce == "" || link == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	wvsqnceInt := parseWeights(wvsqnce)
	archiveMutex.Lock()
	defer archiveMutex.Unlock()

	rows, err := db.Query("SELECT weights FROM archive")
	if err != nil {
		log.Printf("Database query error: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	matchFound := false
	for rows.Next() {
		var weightsStr string
		if err := rows.Scan(&weightsStr); err != nil {
			log.Printf("Database row scan error: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		weights := parseWeights(weightsStr)

		if waveSequenceCohesion(weights, wvsqnceInt) {
			matchFound = true
			break
		}
	}

	if matchFound {
		log.Println("Image archive already exists")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintln(w, "Image archive already exists")
	} else {
		_, err := db.Exec("INSERT INTO archive (links, weights) VALUES (?, ?)", link, wvsqnce)
		if err != nil {
			log.Printf("Database insert error: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, "Image successfully archived")
	}
}







func main() {
    runtime.GOMAXPROCS(runtime.NumCPU())

    db, err := sql.Open("sqlite3", "archive.db")
    if err != nil {
        log.Fatal("Database connection error:", err)
    }
    defer db.Close()

    tokenDB, err := sql.Open("sqlite3", "token.db")
    if err != nil {
        log.Fatal("Token database connection error:", err)
    }
    defer tokenDB.Close()

    _, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS archive (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            links TEXT,
            weights TEXT
        )
    `)
    if err != nil {
        log.Fatal("Archive database table creation error:", err)
    }

    _, err = tokenDB.Exec(`
        CREATE TABLE IF NOT EXISTS tokens (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            customValue INTEGER,
            paymentID TEXT
        )
    `)
    if err != nil {
        log.Fatal("Token database table creation error:", err)
    }

    addr := "127.0.0.1:7777"
    fmt.Printf("Server listening on %s...\n", addr)

    router := mux.NewRouter()
    router.Use(loggingMiddleware)
    router.Use(recoveryMiddleware)

    allowedMethods := handlers.AllowedMethods([]string{"GET", "POST"})
    allowedHeaders := handlers.AllowedHeaders([]string{"Content-Type"})
    allowedOriginsHandler := handlers.AllowedOrigins(allowedOrigins)
    corsHandler := handlers.CORS(allowedOriginsHandler, allowedMethods, allowedHeaders)(router)

    router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "index.html")
    })

    router.HandleFunc("/l", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "1.html")
    })

    router.HandleFunc("/ll", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "2.html")
    })

    router.HandleFunc("/lll", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "3.html")
    })

    router.HandleFunc("/llll", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "4.html")
    })

    router.HandleFunc("/snapscan_webhook", func(w http.ResponseWriter, r *http.Request) {
        SnapScanWebhookHandler(w, r)
    }).Methods(http.MethodPost)

    router.HandleFunc("/COLLECTION", func(w http.ResponseWriter, r *http.Request) {
        CollectionHandler(w, r)
    }).Methods(http.MethodPost)

    router.HandleFunc("/COLLECTED", func(w http.ResponseWriter, r *http.Request) {
        CollectedHandler(w, r)
    }).Methods(http.MethodPost)

    router.HandleFunc("/NEW_CLIENT", func(w http.ResponseWriter, r *http.Request) {
        NewClientHandler(w, r, tokenDB)
    }).Methods(http.MethodPost)

    router.HandleFunc("/FRAME", func(w http.ResponseWriter, r *http.Request) {
        handleFrameRequest(w, r, db)
    }).Methods(http.MethodPost)

    router.HandleFunc("/ARCHIVE", func(w http.ResponseWriter, r *http.Request) {
        handleArchiveRequest(w, r, db)
    }).Methods(http.MethodPost)

    server := &http.Server{
        Addr:    addr,
        Handler: corsHandler,
    }
    log.Fatal(server.ListenAndServe())
}