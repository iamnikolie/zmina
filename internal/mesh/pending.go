package mesh

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// TypedReportPrefix reserves the signed body format that requires reports_v1 on every transport hop.
const TypedReportPrefix = "[zmina-report-v1]"

// TypedPublicationPrefix carries ordered publications under authenticated reports_v2 support.
const TypedPublicationPrefix = "[zmina-report-v2]"

// TellReceiptPrefix marks a signed receipt for a plain line: handed only to a peer that authenticated
// tell_receipts, since an older node would take it for a plain line.
const TellReceiptPrefix = "[zmina-tell-receipt-v1]"

// IsTellReceipt reports a body in the tell receipt format.
func IsTellReceipt(body string) bool { return strings.HasPrefix(body, TellReceiptPrefix) }

// ReportWireVersion recognizes legacy queued publications too: their signed content_hash already
// changes revision semantics even if an earlier writer used the v1 prefix.
func ReportWireVersion(body string) int {
	prefix, version := TypedReportPrefix, 1
	if strings.HasPrefix(body, TypedPublicationPrefix) {
		prefix, version = TypedPublicationPrefix, 2
	} else if !strings.HasPrefix(body, TypedReportPrefix) {
		return 0
	}
	var header struct {
		Version int `json:"version"`
		Report  struct {
			ContentHash string `json:"content_hash"`
		} `json:"report"`
		Receipt struct {
			ReportVersion int `json:"report_version"`
		} `json:"receipt"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(body, prefix)), &header) == nil {
		if header.Version > version {
			version = header.Version
		}
		if (header.Report.ContentHash != "" || header.Receipt.ReportVersion >= 2) && version < 2 {
			version = 2
		}
	}
	return version
}

// PendingError reads a transport observation, never a delivery receipt or an authorization.
func (n *Node) PendingError(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid envelope ID")
	}
	var problem string
	err := readJSON(n.path("pending", id+".json"), &problem)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return problem, err
}

func (n *Node) setPendingError(id, problem string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid envelope ID")
	}
	path := n.path("pending", id+".json")
	if problem == "" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(n.path("pending"), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(problem)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(n.path("pending"), ".pending-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
