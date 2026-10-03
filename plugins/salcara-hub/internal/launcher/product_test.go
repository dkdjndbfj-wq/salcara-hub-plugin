package launcher

import "testing"

func TestSignedFeedRejectsAnotherProduct(t *testing.T) {
	f := newFixture(t)
	defer f.cancel()
	for _, product := range []string{"", "salcara-personal-hub", "salcara-hub-plugin"} {
		feed := testFeed(f.binary)
		feed.Product = product
		if _, err := verifyFeed(fixtureFeed(t, f.private, feed), f.public); err == nil {
			t.Fatalf("accepted signed feed for product %q", product)
		}
	}
	if _, err := verifyFeed(f.feed, f.public); err != nil {
		t.Fatal("rejected the standalone product", err)
	}
}
