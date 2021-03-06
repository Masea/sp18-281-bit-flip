package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/durango/go-credit-card"
	"github.com/gocql/gocql"
	"github.com/gorilla/mux"
	"github.com/scylladb/gocqlx"
	"github.com/scylladb/gocqlx/qb"
	"cmpe281/common/output"
	"cmpe281/common/parse"
	"cmpe281/common"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

func main() {
	var wait time.Duration
	var dbuser, dbpass, dbkeyspace, dbhosts, ip, port string
	var debug bool
	flag.DurationVar(&wait, "graceful-timeout", time.Second*15,
		"the duration for which the server gracefully wait for existing connections to finish - e.g. 15s or 1m")
	flag.StringVar(&dbuser, "dbuser", "starbucks",
		"the username for cassandra database connection")
	flag.StringVar(&dbpass, "dbpass", "",
		"the password for cassandra database connection")
	flag.StringVar(&dbkeyspace, "dbkeyspace", "starbucks",
		"the keyspace for cassandra database connection")
	flag.StringVar(&dbhosts, "dbhosts", "54.176.100.87,54.241.192.98",
		"the hosts (comma separated) for cassandra database connection")
	flag.StringVar(&ip, "ip", "0.0.0.0", "ip address to listen on")
	flag.StringVar(&port, "port", "8080", "port to listen on")
	flag.BoolVar(&debug, "debug", false, "run server in debug mode")
	flag.Parse()

	cluster := gocql.NewCluster(parse.SplitCommaSeparated(dbhosts)...)
	cluster.Keyspace = dbkeyspace
	cluster.Timeout = 5 * time.Second
	cluster.Authenticator = gocql.PasswordAuthenticator{
		Username: dbuser,
		Password: dbpass,
	}

	cluster.ReconnectionPolicy = &gocql.ConstantReconnectionPolicy{
		MaxRetries: 10,
		Interval: 5 * time.Minute,
	}
	cluster.RetryPolicy = &gocql.DowngradingConsistencyRetryPolicy{
		ConsistencyLevelsToTry: []gocql.Consistency {
			gocql.Quorum,
			gocql.LocalQuorum,
			gocql.One,
		},
	}
	cluster.IgnorePeerAddr = true

	log.Printf("Connecting to Cassandra...")
	session, err := cluster.CreateSession()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Done")

	server := &Server{
		Cassandra: session,
	}

	router := mux.NewRouter()

	// Health Check Handler
	router.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
		return
	})

	// Root API Handler
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Starbcuks Payments API")
		return
	})

	// Payment API Handler
	{
		paymentRouter := router.PathPrefix("/payments").Subrouter()
		paymentRouter.Use(common.AuthMiddleware(debug))
		paymentRouter.HandleFunc("", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case "GET":
				server.ListPayments(w, r)
			case "POST":
				server.CreatePayment(w, r)
			case "DELETE":
				server.DeletePayment(w, r)
			case "PUT", "PATCH":
				server.UpdatePayment(w, r)
			default:
				output.WriteErrorMessage(w, http.StatusMethodNotAllowed, "Method Not Supported")
			}
		})
		paymentRouter.HandleFunc("/{payment_id}", server.GetPayment)
	}

	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", ip, port),
		WriteTimeout: time.Second * 15,
		ReadTimeout:  time.Second * 15,
		IdleTimeout:  time.Second * 60,
		Handler:      router, // Pass our instance of gorilla/mux in.
	}

	// Run our server in a goroutine so that it doesn't block.
	go func() {
		log.Printf("Binding to %s:%s", ip, port)
		if err := srv.ListenAndServe(); err != nil {
			log.Println(err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	// Block until SIGINT (Ctrl+C) is received, then begin shutdown.
	<-c

	log.Printf("Server beginning shutdown")
	session.Close()

	// Create a deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	// Wait until all connections are finished or until timeout
	srv.Shutdown(ctx)
	log.Println("Server shut down successfully")
	os.Exit(0)
}

type Server struct {
	Cassandra *gocql.Session
}

// Returns pagination details of the Request's query string
func GetPagination(r *http.Request) (uint, string) {
	var limit uint
	var pageToken string

	vars := r.URL.Query()
	limit64, err := strconv.ParseUint(vars.Get("limit"), 10, 32)
	if err == nil {
		limit = uint(limit64)
	} else {
		limit = 10
	}
	pageToken = vars.Get("pageToken")

	return limit, pageToken
}

func (srv *Server) ListPayments(w http.ResponseWriter, r *http.Request) {
	// Parse Query Parameters
	limit, pageToken := GetPagination(r)

	// Parse Query Inputs
	querySelectors := qb.M{
		"user_id":    nil,
		"payment_id": nil,
	}
	if userId, err := gocql.ParseUUID(common.GetUserId(r)); err == nil {
		querySelectors["user_id"] = userId
	} else {
		output.WriteErrorMessage(w, http.StatusUnauthorized, "Unable to Authenticate")
		return
	}
	if paymentId, err := gocql.ParseUUID(pageToken); err == nil {
		querySelectors["payment_id"] = paymentId
	}

	// Set up Query
	query, names := qb.Select("payments").
		Where(qb.Eq("user_id"), qb.Gt("payment_id")).
		Limit(limit).
		ToCql()
	q := gocqlx.Query(srv.Cassandra.Query(query), names).BindMap(querySelectors)

	// Execute Query
	var payments []PaymentDetails
	if err := gocqlx.Iter(q.Query).Unsafe().Select(&payments); err != nil {
		log.Println(err)
		output.WriteErrorMessage(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// Build Next Page Token
	var nextPageToken *gocql.UUID
	if len(payments) > 0 {
		nextPageToken = &payments[len(payments)-1].PaymentId
	}

	// Transform Output to JSON
	output.WriteJson(w, &ListPaymentsResult{
		Payments:      payments,
		NextPageToken: nextPageToken,
	})
}

func (srv *Server) CreatePayment(w http.ResponseWriter, r *http.Request) {
	var payment *PaymentDetails
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&payment); err != nil {
		log.Println(err)
		output.WriteErrorMessage(w, http.StatusBadRequest, "Bad Request")
		return
	}

	if userId, err := gocql.ParseUUID(common.GetUserId(r)); err == nil {
		payment.UserId = userId
	} else {
		log.Println(err)
		output.WriteErrorMessage(w, http.StatusUnauthorized, "Unable to Authenticate")
		return
	}
	payment.PaymentId = gocql.TimeUUID()

	card := &creditcard.Card{
		Number: payment.CardDetails.Number,
		Cvv:    "111",
		Month:  payment.CardDetails.ExpMonth,
		Year:   payment.CardDetails.ExpYear,
	}
	if err := card.Validate( /* allowTestNumbers= */ true); err != nil {
		payment.Status = "Declined" // Invalid Card --> Declined :(
	} else {
		payment.Status = "Approved" // Valid Card --> Approved :)
	}

	query, names := qb.Insert("payments").Columns("card_details", "billing_details", "user_id", "payment_id", "status", "amount").ToCql()
	q := gocqlx.Query(srv.Cassandra.Query(query), names).BindStruct(payment)

	if err := q.ExecRelease(); err != nil {
		log.Println(err)
		output.WriteErrorMessage(w, http.StatusInternalServerError, "Failed to create Payment")
		return
	}

	// Transform Output to JSON
	output.WriteJson(w, payment)
	return
}

func (srv *Server) GetPayment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	// Parse Query Inputs
	querySelectors := qb.M{
		"user_id":    nil,
		"payment_id": nil,
	}
	if userId, err := gocql.ParseUUID(common.GetUserId(r)); err == nil {
		querySelectors["user_id"] = userId
	} else {
		output.WriteErrorMessage(w, http.StatusUnauthorized, "Unable to Authenticate")
		return
	}
	if paymentId, err := gocql.ParseUUID(vars["payment_id"]); err == nil {
		querySelectors["payment_id"] = paymentId
	} else {
		output.WriteErrorMessage(w, http.StatusBadRequest, "Payment ID not provided")
		return
	}

	// Set up Query
	query, names := qb.Select("payments").
		Where(qb.Eq("user_id"), qb.Eq("payment_id")).
		ToCql()
	q := gocqlx.Query(srv.Cassandra.Query(query), names).BindMap(querySelectors)

	// Execute Query
	var payment PaymentDetails
	if err := gocqlx.Iter(q.Query).Unsafe().Get(&payment); err != nil {
		switch err {
		case gocql.ErrNotFound:
			output.WriteErrorMessage(w, http.StatusNotFound, "Payment not found")
			return
		default:
			log.Println(err)
			output.WriteErrorMessage(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
	}

	// Transform Output to JSON
	output.WriteJson(w, payment)
	return
}

func (srv *Server) DeletePayment(w http.ResponseWriter, r *http.Request) {
	// Payment Deletion not Supported -- This may be used for Payment *Reversal* in the future
	// It may be worth implementing reversals via a specific endpoint rather than Method DELETE
	output.WriteErrorMessage(w, http.StatusMethodNotAllowed, "Payments may not be deleted")
	return
}

func (srv *Server) UpdatePayment(w http.ResponseWriter, r *http.Request) {
	// Payment Modification not Supported -- Payment state should only change via a limited
	// number of exposed methods. This may include payment reversal and payment processing status
	// (for asyncronous payments) in future versions.
	output.WriteErrorMessage(w, http.StatusMethodNotAllowed, "Payments may not be modified")
	return
}