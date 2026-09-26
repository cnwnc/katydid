package discord

// Candidate is one katyd /resolve match, a release group.
type Candidate struct {
	ReleaseID   string `json:"release_id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Date        string `json:"date"`
	TrackCount  int    `json:"track_count"`
	PrimaryType string `json:"primary_type"`
}

type resolveResponse struct {
	Candidates []Candidate `json:"candidates"`
	Auto       bool        `json:"auto"`
}

// Want is the slice of a fetchd want the bot renders.
type Want struct {
	ID            string     `json:"id"`
	Artist        string     `json:"artist"`
	Album         string     `json:"album"`
	Year          int        `json:"year,omitempty"`
	State         string     `json:"state"`
	Error         string     `json:"error,omitempty"`
	AlbumID       string     `json:"album_id,omitempty"`
	DecisionToken string     `json:"decision_token,omitempty"`
	Notes         []string   `json:"notes,omitempty"`
	Enqueued      []struct{} `json:"enqueued,omitempty"`
	Downloaded    int        `json:"downloaded,omitempty"`
}

type wantResponse struct {
	Want Want `json:"want"`
}

type wantsResponse struct {
	Wants []Want `json:"wants"`
}

// Album is the slice of a katyd library album the bot needs: the names
// and release id musicbrainz gave it, not what the user typed.
type Album struct {
	ID   string `json:"id"`
	Meta struct {
		AlbumArtist string `json:"albumartist"`
		Album       string `json:"album"`
		ReleaseID   string `json:"release_id"`
	} `json:"meta"`
}

// Query is the katyd /resolve specifier.
type Query struct {
	Artist string
	Album  string
	Year   int
	MBID   string
	Limit  int
}

// AddWant is the fetchd POST /wants body. MBID and Group are mutually exclusive.
type AddWant struct {
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Year   int    `json:"year,omitempty"`
	MBID   string `json:"mbid,omitempty"`
	Group  string `json:"group,omitempty"`
}
