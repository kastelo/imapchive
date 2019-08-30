package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/calmh/imapchive/db"
)

const (
	extension = ".imapchive"
)

var (
	version     = "unknown-dev"
	fullVersion = fmt.Sprintf("imapchive %s (%s-%s)", version, runtime.GOOS, runtime.GOARCH)
)

var progress struct {
	toScan  int64
	scanned int64
	fetched int64
	labels  int64
}

func main() {
	kingpin.Version(fullVersion)

	flagEmail := kingpin.Flag("email", "Email address").Envar("IMAP_EMAIL").String()
	flagPassword := kingpin.Flag("password", "Password").Envar("IMAP_PASSWORD").String()
	flagServer := kingpin.Flag("server", "Server address").Envar("IMAP_SERVER").Default("imap.gmail.com:993").String()

	cmdFetch := kingpin.Command("fetch", "Fetch new mail")
	flagMailbox := cmdFetch.Arg("mailbox", "Mailbox name").Required().String()
	flagConcurrency := cmdFetch.Flag("concurrency", "Number of parallel fetch threads").Default("4").Int()

	cmdMbox := kingpin.Command("mbox", "Write an MBOX file with all messages to stdout")
	argFile := cmdMbox.Arg("file", "Archive file").Required().String()

	cmdList := kingpin.Command("list", "List available mailboxes")

	switch kingpin.Parse() {
	case cmdList.FullCommand():
		cl, err := Client(*flagServer, *flagEmail, *flagPassword, "")
		if err != nil {
			fmt.Printf("Listing mailboxes: %+v\n", err)
			os.Exit(1)
		}
		mailboxes, err := cl.Mailboxes()
		if err != nil {
			fmt.Printf("Listing mailboxes: %+v\n", err)
			os.Exit(1)
		}
		for _, mb := range mailboxes {
			fmt.Println(mb)
		}

	case cmdFetch.FullCommand():
		log.Println("Opening archive")
		dbName := strings.Replace(*flagMailbox, "/", "_", -1) + extension
		db, err := db.Open(dbName)
		if err != nil {
			log.Fatalf("Opening archive: %+v", err)
		}

		log.Printf("Have %d messages", db.Size())
		uids := findNewUIDs(*flagServer, *flagEmail, *flagPassword, *flagMailbox, db)

		var wg sync.WaitGroup
		for i := 1; i <= *flagConcurrency; i++ {
			wg.Add(1)
			go func(i int) {
				fetchAndStore(*flagServer, *flagEmail, *flagPassword, *flagMailbox, i, db, uids)
				wg.Done()
			}(i)
		}

		go func() {
			for {
				time.Sleep(10 * time.Second)
				buf := new(bytes.Buffer)
				fmt.Fprintf(buf, "%d of %d scanned", atomic.LoadInt64(&progress.scanned), atomic.LoadInt64(&progress.toScan))
				if fetched := atomic.LoadInt64(&progress.fetched); fetched > 0 {
					fmt.Fprintf(buf, ", %d fetched", fetched)
				}
				if labeled := atomic.LoadInt64(&progress.labels); labeled > 0 {
					fmt.Fprintf(buf, ", %d labelupdated", labeled)
				}
				log.Println(buf.String())
			}
		}()

		wg.Wait()

		err = db.WriteClose()
		if err != nil {
			log.Fatalf("Save to database: %+v", err)
		}

	case cmdMbox.FullCommand():
		db, err := db.Open(*argFile)
		if err != nil {
			fmt.Printf("Opening archive: %+v\n", err)
			os.Exit(1)
		}

		mbox(db, os.Stdout)
	}
}

func findNewUIDs(server, email, password, mailbox string, db *db.DB) chan msg {
	client, err := Client(server, email, password, mailbox)
	if err != nil {
		log.Fatalf("Find new UIDs: %+v", err)
	}

	atomic.StoreInt64(&progress.toScan, int64(client.Mailbox.Messages))

	const step = 1000
	out := make(chan msg, step)
	go func() {
		begin := uint32(1)
		for begin < client.Mailbox.Messages {
			end := begin + step - 1
			if end > client.Mailbox.Messages {
				end = client.Mailbox.Messages
			}

			msgs, err := client.MsgIDSearch(begin, end)
			if err != nil {
				log.Fatalf("Find new UIDs: %+v", err)
			}

			begin += step
			atomic.AddInt64(&progress.scanned, int64(len(msgs)))

			for _, msg := range msgs {
				if !db.Have(msg.UID) {
					out <- msg
				} else if !sliceEquals(db.Labels(msg.UID), msg.Labels) {
					if err := db.SetLabels(msg.UID, msg.Labels); err != nil {
						log.Fatalf("Save to database: %+v", err)
					}
					atomic.AddInt64(&progress.labels, 1)
				}
			}
		}
		close(out)
	}()

	return out
}

func sliceEquals(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fetchAndStore(server, email, password, mailbox string, id int, db *db.DB, msgids chan msg) {
	client, err := Client(server, email, password, mailbox)
	if err != nil {
		log.Fatalf("Fetch and store: %+v", err)
	}

loop:
	for {
		select {
		case msgid, ok := <-msgids:
			if !ok {
				break loop
			}

			body, err := client.GetMail(msgid.UID)
			if err != nil {
				log.Fatalf("Fetch and store: %+v", err)
			}

			err = db.WriteMessage(msgid.UID, body, msgid.Labels)
			if err != nil {
				log.Fatalf("Fetch and store: %+v", err)
			}

			atomic.AddInt64(&progress.fetched, 1)
		}
	}
}

func mbox(db *db.DB, wr io.Writer) {
	var nwritten int
	nl := []byte("\n")
	from := []byte("From ")
	esc := []byte(">")

	bwr := bufio.NewWriter(wr)

	for {
		rec, err := db.ReadRecord()
		if err == io.EOF {
			break
		}

		if !db.Have(rec.MessageID) {
			// Message has been deleted
			continue
		}

		bwr.Write([]byte("From MAILER-DAEMON Thu Jan  1 01:00:00 1970\n"))
		if labels := db.Labels(rec.MessageID); len(labels) > 0 {
			fmt.Fprintf(bwr, "X-Gmail-Labels: %s\n", strings.Join(labels, ","))
		}
		sc := bufio.NewScanner(bytes.NewReader(rec.MessageData))
		for sc.Scan() {
			line := sc.Bytes()
			if bytes.HasPrefix(line, from) {
				bwr.Write(esc)
			}
			bwr.Write(line)
			bwr.Write(nl)
		}
		bwr.Write(nl)
		bwr.Flush()

		nwritten++
	}

	log.Printf("Wrote %d messages to stdout", nwritten)
}
