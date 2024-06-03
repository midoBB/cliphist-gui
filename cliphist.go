package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "embed"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"fyne.io/systray"
	bolt "go.etcd.io/bbolt"
	"go.senan.xyz/flagconf"
	_ "golang.org/x/image/bmp"
)

//go:embed version.txt
var version string

//go:embed clipboard.png
var AppIcon []byte

const dbp = "$HOME/.cache/cliphist/db"

// createLockFile creates and locks a file to ensure only one instance of the systray application is running.
func createLockFile(lockFilePath string) (*os.File, error) {
	file, err := os.OpenFile(lockFilePath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}

	// Try to obtain an exclusive lock.
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("another systray instance is already running")
	}

	return file, nil
}

//nolint:errcheck
func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  $ %s <store|list|decode|delete|delete-query|wipe|version>\n", flag.CommandLine.Name())
		fmt.Fprintf(flag.CommandLine.Output(), "options:\n")
		flag.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(flag.CommandLine.Output(), "  -%s (default %s)\n", f.Name, f.DefValue)
			fmt.Fprintf(flag.CommandLine.Output(), "    %s\n", f.Usage)
		})
	}

	maxItems := flag.Uint64("max-items", 750, "maximum number of items to store")
	maxDedupeSearch := flag.Uint64("max-dedupe-search", 100, "maximum number of last items to look through when finding duplicates")
	previewWidth := flag.Uint("preview-width", 100, "maximum number of characters to preview")
	// dbPath := flag.String("db-path", "$XDG_CACHE_HOME/cliphist/db", "path to db")
	configPath := flag.String("config-path", "$XDG_CONFIG_HOME/cliphist/config", "overwrite config path to use instead of cli flags")

	flag.Parse()
	flagconf.ParseEnv()
	flagconf.ParseConfig(*configPath)

	dbPath := os.ExpandEnv(dbp)
	socketPath := "/tmp/cliphist.sock"
	lockFilePath := "/tmp/myapp.lock"
	var err error
	switch flag.Arg(0) {
	case "store":
		switch os.Getenv("CLIPBOARD_STATE") { // from man wl-clipboard
		case "sensitive":
		case "clear":
			err = deleteLast(dbPath, socketPath)
		default:
			err = store(dbPath, os.Stdin, *maxDedupeSearch, *maxItems, socketPath)
		}
	case "list":
		err = list(dbPath, os.Stdout, *previewWidth)
	case "decode":
		err = decode(dbPath, os.Stdin, os.Stdout, flag.Arg(1))
	case "delete-query":
		err = deleteQuery(dbPath, flag.Arg(1), socketPath)
	case "delete":
		err = delete(dbPath, os.Stdin, socketPath)
	case "wipe":
		err = wipe(dbPath, socketPath)
	case "version":
		fmt.Fprintf(flag.CommandLine.Output(), "%s\t%s\n", "version", strings.TrimSpace(version))
		flag.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(flag.CommandLine.Output(), "%s\t%s\n", f.Name, f.Value)
		})
	default:
		systray.Run(func() {
			onReady(socketPath, lockFilePath)
		}, func() {})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()
	// Read the incoming message
	buffer := make([]byte, 1024)
	_, err := conn.Read(buffer)
	if err != nil {
		fmt.Println("Error reading:", err.Error())
		return
	}
	loadSysTrayItems()
}

func loadSysTrayItems() {
	dbPath := os.ExpandEnv(dbp)
	systray.ResetMenu()
	result, _ := listWithLimit(dbPath, 10, 50)
	for _, v := range result {
		addClipItem(v, dbPath)
	}
	systray.AddSeparator()
	addQuitItem()
}

func startServer(socketPath string) {
	// Remove the socket file if it already exists
	if err := os.RemoveAll(socketPath); err != nil {
		fmt.Println("Error removing existing socket file:", err.Error())
		os.Exit(1)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Println("Error starting server:", err.Error())
		os.Exit(1)
	}
	defer listener.Close()

	fmt.Println("Server started, waiting for connections...")
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err.Error())
			continue
		}
		go handleConnection(conn)
	}
}

func addQuitItem() {
	mQuit := systray.AddMenuItem("Quit", "Quit the whole app")
	mQuit.Enable()
	go func() {
		<-mQuit.ClickedCh
		fmt.Println("Requesting quit")
		systray.Quit()
		fmt.Println("Finished quitting")
	}()
	systray.AddSeparator()
}

func onReady(socketPath string, lockFilePath string) {
	// Ensure the socket is not already in use
	if err := os.RemoveAll(socketPath); err != nil {
		fmt.Println("Error removing existing socket file:", err.Error())
		os.Exit(1)
	}
	// Ensure single systray instance
	lockFile, err := createLockFile(lockFilePath)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer lockFile.Close()
	defer os.Remove(lockFilePath)
	go startServer(socketPath)
	loadSysTrayItems()
	systray.SetTitle("Cliphist GUI")
	systray.SetIcon(AppIcon)
	systray.SetTooltip("Clipboard history manager for Wayland")
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	<-signalChannel
}

func sendNotification(socketPath string) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Println("Error connecting:", err.Error())
		return
	}
	defer conn.Close()

	message := "Command executed in another mode"
	_, err = conn.Write([]byte(message))
	if err != nil {
		fmt.Println("Error sending message:", err.Error())
	}
}

func addClipItem(v *PreviewItem, dbpath string) {
	mClipItem := systray.AddMenuItem(v.Value, "")
	mClipItem.Enable()
	go func(v *PreviewItem) {
		<-mClipItem.ClickedCh

		// Create a pipe
		r, w := io.Pipe()

		// Start wl-copy command
		cmd := exec.Command("wl-copy")
		cmd.Stdin = r

		// Start the wl-copy command in a goroutine
		go func() {
			if err := cmd.Run(); err != nil {
				fmt.Printf("wl-copy command failed: %v\n", err)
			}
			r.Close()
		}()

		// Call decodeFromId with the write end of the pipe
		err := decodeFromId(dbpath, v.Key, w)
		if err != nil {
			fmt.Printf("decodeFromId failed: %v\n", err)
		}
		w.Close()
	}(v)
}

func store(dbPath string, in io.Reader, maxDedupeSearch, maxItems uint64, socketPath string) error {
	input, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if len(input) > 5*1e6 { // don't store >5MB
		return nil
	}

	db, err := initDB(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	if len(bytes.TrimSpace(input)) == 0 {
		return nil
	}
	tx, err := db.Begin(true)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))

	if err := deduplicate(b, input, maxDedupeSearch); err != nil {
		return fmt.Errorf("deduplicating: %w", err)
	}
	id, err := b.NextSequence()
	if err != nil {
		return fmt.Errorf("getting next sequence: %w", err)
	}
	if err := b.Put(itob(id), input); err != nil {
		return fmt.Errorf("insert stdin: %w", err)
	}
	if err := trimLength(b, maxItems); err != nil {
		return fmt.Errorf("trimming length: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	sendNotification(socketPath)
	return nil
}

// trim the store's size to a number of max items. manually counting
// seen items because we can't rely on sequence numbers when items can
// be deleted when deduplicating
func trimLength(b *bolt.Bucket, maxItems uint64) error {
	c := b.Cursor()
	var seen uint64
	for k, _ := c.Last(); k != nil; k, _ = c.Prev() {
		if seen < maxItems {
			seen++
			continue
		}
		if err := b.Delete(k); err != nil {
			return fmt.Errorf("delete :%w", err)
		}
		seen++
	}
	return nil
}

func deduplicate(b *bolt.Bucket, input []byte, maxDedupeSearch uint64) error {
	c := b.Cursor()
	var seen uint64
	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		if seen > maxDedupeSearch {
			break
		}
		if !bytes.Equal(v, input) {
			seen++
			continue
		}
		if err := b.Delete(k); err != nil {
			return fmt.Errorf("delete :%w", err)
		}
		seen++
	}
	return nil
}

type PreviewItem struct {
	Key   uint64
	Value string
}

func listWithLimit(dbPath string, limit uint, previewWidth uint) ([]*PreviewItem, error) {
	db, err := initDBReadOnly(dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(false)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))
	c := b.Cursor()
	res := make([]*PreviewItem, 0)
	i := uint(0)
	for k, v := c.Last(); k != nil && i < limit; {
		res = append(res, &PreviewItem{
			Key:   btoi(k),
			Value: previewNoIndex(v, previewWidth),
		})
		if k == nil || i >= limit {
			break
		}
		k, v = c.Prev()
		i++
	}
	return res, nil
}

func list(dbPath string, out io.Writer, previewWidth uint) error {
	db, err := initDBReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(false)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))
	c := b.Cursor()
	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		fmt.Fprintln(out, preview(btoi(k), v, previewWidth))
	}
	return nil
}

const fieldSep = "\t"

func extractID(input string) (uint64, error) {
	idStr, _, _ := strings.Cut(input, fieldSep)
	if idStr == "" {
		return 0, fmt.Errorf("input not prefixed with id")
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, fmt.Errorf("converting id: %w", err)
	}
	return uint64(id), nil
}

func decode(dbPath string, in io.Reader, out io.Writer, input string) error {
	if input == "" {
		inp, err := io.ReadAll(in)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		input = string(inp)
	}
	id, err := extractID(input)
	if err != nil {
		return fmt.Errorf("extracting id: %w", err)
	}

	db, err := initDBReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(false)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))
	v := b.Get(itob(id))
	if _, err := out.Write(v); err != nil {
		return fmt.Errorf("writing out: %w", err)
	}
	return nil
}

func decodeFromId(dbPath string, id uint64, out io.Writer) error {
	db, err := initDBReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(false)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))
	v := b.Get(itob(id))
	if _, err := out.Write(v); err != nil {
		return fmt.Errorf("writing out: %w", err)
	}
	return nil
}

func deleteQuery(dbPath string, query string, socketPath string) error {
	if query == "" {
		return fmt.Errorf("please provide a query")
	}

	db, err := initDB(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(true)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if bytes.Contains(v, []byte(query)) {
			_ = b.Delete(k)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	sendNotification(socketPath)
	return nil
}

func deleteLast(dbPath string, socketPath string) error {
	db, err := initDB(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(true)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))
	c := b.Cursor()
	k, _ := c.Last()
	_ = b.Delete(k)

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	sendNotification(socketPath)
	return nil
}

func delete(dbPath string, in io.Reader, socketPath string) error {
	input, err := io.ReadAll(in) // drain stdin before opening and locking db
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	db, err := initDB(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(true)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for sc := bufio.NewScanner(bytes.NewReader(input)); sc.Scan(); {
		id, err := extractID(sc.Text())
		if err != nil {
			return fmt.Errorf("extract id: %w", err)
		}
		b := tx.Bucket([]byte(bucketKey))
		if err := b.Delete(itob(id)); err != nil {
			return fmt.Errorf("delete key: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	sendNotification(socketPath)
	return nil
}

func wipe(dbPath string, socketPath string) error {
	db, err := initDB(dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin(true)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	b := tx.Bucket([]byte(bucketKey))
	c := b.Cursor()
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		_ = b.Delete(k)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	sendNotification(socketPath)
	return nil
}

const bucketKey = "b"

func initDB(path string) (*bolt.DB, error)         { return initDBOption(path, false) }
func initDBReadOnly(path string) (*bolt.DB, error) { return initDBOption(path, true) }

func initDBOption(path string, ro bool) (*bolt.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	// https://github.com/etcd-io/bbolt/issues/98
	if ro {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("please store something first")
		}
	}

	db, err := bolt.Open(path, 0644, &bolt.Options{
		ReadOnly: ro,
		Timeout:  1 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if db.IsReadOnly() {
		return db, nil
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketKey))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("init bucket: %w", err)
	}
	return db, nil
}

func previewNoIndex(data []byte, width uint) string {
	if config, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		return fmt.Sprintf("[[ binary data %s %s %dx%d ]]",
			sizeStr(len(data)), format, config.Width, config.Height)
	}
	prev := string(data)
	prev = strings.TrimSpace(prev)
	prev = strings.Join(strings.Fields(prev), " ")
	prev = trunc(prev, int(width), "…")
	return prev
}

func preview(index uint64, data []byte, width uint) string {
	if config, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		return fmt.Sprintf("%d%s[[ binary data %s %s %dx%d ]]",
			index, fieldSep, sizeStr(len(data)), format, config.Width, config.Height)
	}
	prev := string(data)
	prev = strings.TrimSpace(prev)
	prev = strings.Join(strings.Fields(prev), " ")
	prev = trunc(prev, int(width), "…")
	return fmt.Sprintf("%d%s%s", index, fieldSep, prev)
}

func trunc(in string, max int, ellip string) string {
	runes := []rune(in)
	if len(runes) > max {
		return string(runes[:max]) + ellip
	}
	return in
}

func min(a, b int) int { //nolint:unused // we still support go1.19
	if a < b {
		return a
	}
	return b
}

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func btoi(v []byte) uint64 {
	return binary.BigEndian.Uint64(v)
}

func sizeStr(size int) string {
	units := []string{"B", "KiB", "MiB"}

	var i int
	fsize := float64(size)
	for fsize >= 1024 && i < len(units)-1 {
		fsize /= 1024
		i++
	}
	return fmt.Sprintf("%.0f %s", fsize, units[i])
}
