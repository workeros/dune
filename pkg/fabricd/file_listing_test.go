package fabricd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryPagesHaveBoundedSortedCandidates(t *testing.T) {
	root := t.TempDir()
	const count = 4300
	for i := count - 1; i >= 0; i-- {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := listFilePage(context.Background(), root, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 3 || first.NextCursor == "" || first.Items[0].Name != "file-0000" || first.Items[2].Name != "file-0002" {
		t.Fatal("incorrect first page", first)
	}
	second, err := listFilePage(context.Background(), root, first.NextCursor, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 3 || second.Items[0].Name != "file-0003" || second.Items[2].Name != "file-0005" {
		t.Fatal("incorrect second page", second)
	}
	for _, item := range append(first.Items, second.Items...) {
		if item.ContentHash != "" || item.Revision != "" {
			t.Fatal("directory page generated a file revision", item)
		}
	}
	if _, err := listFilePage(context.Background(), t.TempDir(), first.NextCursor, 3); err == nil {
		t.Fatal("directory cursor was accepted for another root")
	}
}

func TestDirectoryScanCancellationDoesNotReturnPartialPage(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "first"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err := listFilePage(ctx, root, "", 1)
	requireFileCode(t, err, "DIRECTORY_SCAN_INCOMPLETE")
	if len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatal("cancelled scan returned a partial sorted page", page)
	}
}

func TestDirectoryPagesKeepEmptyDirectoriesAndLinkIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "empty"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "with\nnewline"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	page, err := listFilePage(context.Background(), root, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || !page.Items[0].IsDir || page.Items[1].IsDir || os.FileMode(page.Items[1].Mode)&os.ModeSymlink == 0 || page.Items[2].Name != "with\nnewline" {
		t.Fatal(page)
	}
	if page.NextCursor != "" {
		t.Fatal("last page advertised more entries", page)
	}
}
