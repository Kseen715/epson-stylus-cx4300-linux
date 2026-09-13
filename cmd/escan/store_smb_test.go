package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"testing"
	"time"
)

// TestSMBStoreRoundTrip talks to a real server, so it only runs when one is
// named. Against a throwaway Samba container:
//
//	docker run -d --rm -p 4455:445 --name smb \
//	    -e "USER=scanuser;secret" -e "SHARE=scans;/share;yes;no;no;scanuser" \
//	    dperson/samba
//	docker exec smb chmod 0777 /share   # the image leaves it root-owned
//	ESCAN_SMB_ADDRESS=//127.0.0.1:4455/scans ESCAN_SMB_USER=scanuser \
//	    ESCAN_SMB_PASSWORD=secret go test ./cmd/escan -run SMBStoreRoundTrip -v
func TestSMBStoreRoundTrip(t *testing.T) {
	address := os.Getenv("ESCAN_SMB_ADDRESS")
	if address == "" {
		t.Skip("set ESCAN_SMB_ADDRESS, ESCAN_SMB_USER and ESCAN_SMB_PASSWORD to run this")
	}
	st, err := newSMBStore(address, os.Getenv("ESCAN_SMB_USER"),
		os.Getenv("ESCAN_SMB_PASSWORD"), os.Getenv("ESCAN_SMB_DOMAIN"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.check(); err != nil {
		t.Fatal(err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 0xff, A: 0xff})
	name := fmt.Sprintf("escan-test-%d.png", time.Now().UnixNano())

	// The same call the scan path makes.
	if err := writePNG(st, name, img); err != nil {
		t.Fatalf("writePNG: %v", err)
	}

	// And the same call the Download link makes.
	f, modTime, err := st.Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if modTime.IsZero() {
		t.Error("no modification time; http.ServeContent needs one")
	}
	got, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding what came back: %v", err)
	}
	if got.Bounds() != img.Bounds() {
		t.Errorf("bounds = %v, want %v", got.Bounds(), img.Bounds())
	}

	// Seeking is what makes range requests work, so prove it on the real file.
	f, _, err = st.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(8, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading after seek: %v", err)
	}
	if len(tail) != len(data)-8 {
		t.Errorf("read %d bytes after seeking 8 into %d", len(tail), len(data))
	}
}
