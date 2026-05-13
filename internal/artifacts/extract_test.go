package artifacts

import (
	"reflect"
	"testing"
)

func TestExtractURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []ExtractedURL
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "no urls",
			in:   "just some prose without links",
			want: nil,
		},
		{
			name: "bare url",
			in:   "Here it is: https://api.devin.ai/v1/attachments/abc.png",
			want: []ExtractedURL{{URL: "https://api.devin.ai/v1/attachments/abc.png", Filename: "abc.png"}},
		},
		{
			name: "trailing punctuation stripped",
			in:   "see https://example.com/path/foo.txt. that's all.",
			want: []ExtractedURL{{URL: "https://example.com/path/foo.txt", Filename: "foo.txt"}},
		},
		{
			name: "markdown image syntax",
			in:   "![screenshot](https://example.com/snap.png)",
			want: []ExtractedURL{{URL: "https://example.com/snap.png", Filename: "snap.png"}},
		},
		{
			name: "dedup",
			in:   "first https://a.example/x.png then https://a.example/x.png again",
			want: []ExtractedURL{{URL: "https://a.example/x.png", Filename: "x.png"}},
		},
		{
			name: "multiple distinct",
			in:   "https://a.example/x.png and https://b.example/y.zip",
			want: []ExtractedURL{
				{URL: "https://a.example/x.png", Filename: "x.png"},
				{URL: "https://b.example/y.zip", Filename: "y.zip"},
			},
		},
		{
			name: "ignore non-http schemes",
			in:   "ftp://example.com/blob and mailto:a@b.com",
			want: nil,
		},
		{
			name: "url-encoded filename",
			in:   "https://example.com/My%20Doc.pdf",
			want: []ExtractedURL{{URL: "https://example.com/My%20Doc.pdf", Filename: "My Doc.pdf"}},
		},
		{
			name: "no path falls back to attachment",
			in:   "https://example.com",
			want: []ExtractedURL{{URL: "https://example.com", Filename: "attachment"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractURLs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractURLs(%q) = %#v\n want %#v", tc.in, got, tc.want)
			}
		})
	}
}
