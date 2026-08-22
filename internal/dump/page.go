package dump

type Page struct {
	Title string `xml:"title"`
	NS    int    `xml:"ns"`
	ID    int    `xml:"id"`
	Text  string `xml:"revision>text"`
}
