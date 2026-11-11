package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// A tiny, deterministic faker. Every person and business is made up.
// Phone numbers use Ofcom's drama range (07700 900000–900999) and emails use
// example.com, so nothing can reach a real person.

var firstNames = []string{
	"Ailsa", "Alasdair", "Amir", "Ava", "Callum", "Catriona", "Chloe", "Ciaran", "Daniel", "Eilidh",
	"Emma", "Euan", "Fatima", "Finlay", "Fraser", "Grace", "Hamish", "Hannah", "Isla", "Jack",
	"Jamie", "Jia", "Kirsty", "Kenji", "Lewis", "Lucy", "Mairi", "Malik", "Megan", "Niamh",
	"Oliver", "Olivia", "Priya", "Rory", "Rosie", "Ruairidh", "Sana", "Scott", "Shona", "Sofia",
	"Tomasz", "Yusuf", "Zainab", "Zofia", "Ewan", "Iona", "Kyle", "Leah", "Mohammed", "Rhona",
}

var lastNames = []string{
	"Anderson", "Baird", "Bell", "Brown", "Campbell", "Chen", "Clark", "Cunningham", "Docherty", "Douglas",
	"Fraser", "Gallacher", "Gordon", "Graham", "Hamilton", "Hughes", "Hussain", "Johnstone", "Kaur", "Kerr",
	"Khan", "Kowalski", "MacDonald", "MacLeod", "McGregor", "McKenzie", "McLean", "Mitchell", "Morrison", "Murray",
	"Nowak", "O'Neill", "Paterson", "Reid", "Robertson", "Ross", "Scott", "Shah", "Sinclair", "Smith",
	"Stewart", "Thomson", "Wallace", "Watson", "Wilson", "Wong", "Young", "Ahmed", "Duncan", "Hay",
}

type sector struct {
	key     string
	weight  float64 // relative booking volume per account
	suffix  []string
	tierMix float64 // chance of being plus or enterprise
}

var sectors = []sector{
	{"florist", 1.6, []string{"Florists", "Flowers", "Floral Studio"}, 0.10},
	{"print_shop", 1.3, []string{"Print", "Printers", "Print & Copy"}, 0.15},
	{"law_firm", 1.1, []string{"Solicitors", "Legal", "& Partners LLP"}, 0.35},
	{"restaurant", 0.9, []string{"Kitchen", "Bistro", "Deli"}, 0.05},
	{"bakery", 1.0, []string{"Bakehouse", "Bakery", "Patisserie"}, 0.05},
	{"pharmacy", 1.4, []string{"Pharmacy", "Chemists"}, 0.20},
	{"estate_agent", 0.7, []string{"Property", "Estates", "Lettings"}, 0.15},
	{"architect", 0.5, []string{"Architects", "Design Studio"}, 0.10},
	{"dental_lab", 0.8, []string{"Dental Lab", "Dental Ceramics"}, 0.10},
	{"gift_shop", 1.2, []string{"Gifts", "Homeware", "Trading Co."}, 0.10},
	{"bike_shop", 0.6, []string{"Cycles", "Bike Works"}, 0.05},
	{"agency", 0.6, []string{"Creative", "Studio", "Agency"}, 0.20},
}

var businessWords = []string{
	"Thistle", "Heather", "Clyde", "Forth", "Kelvin", "Bluebell", "Saltire", "Rowan", "Merchant", "Corran",
	"Birch", "Cairn", "Bothy", "Harbour", "Lochside", "Firth", "Wynd", "Close", "Brae", "Glen",
	"Ochil", "Pentland", "Arran", "Iona", "Tay", "Dalriada", "Old Mill", "Tenement", "Kelpie", "Puffin",
}

func personName(r *rand.Rand) (first, last string) {
	return firstNames[r.IntN(len(firstNames))], lastNames[r.IntN(len(lastNames))]
}

func businessName(r *rand.Rand, s sector) string {
	w := businessWords[r.IntN(len(businessWords))]
	suffix := s.suffix[r.IntN(len(s.suffix))]
	switch r.IntN(3) {
	case 0:
		_, last := personName(r)
		return last + " " + suffix
	case 1:
		w2 := businessWords[r.IntN(len(businessWords))]
		if w2 == w {
			return w + " " + suffix
		}
		return w + " & " + w2 + " " + suffix
	default:
		return w + " " + suffix
	}
}

func phone(r *rand.Rand) string {
	return fmt.Sprintf("07700 900%03d", r.IntN(1000))
}

func email(first, last string, n int) string {
	clean := func(s string) string { return strings.ToLower(strings.NewReplacer("'", "", " ", "").Replace(s)) }
	return fmt.Sprintf("%s.%s%d@example.com", clean(first), clean(last), n)
}

// postcode returns a made-up but well-formed postcode for a city area.
func postcode(r *rand.Rand, prefix string) string {
	const letters = "ABDEFGHJLNPQRSTUWXYZ" // letters allowed in the inward code
	district := 1 + r.IntN(53)
	if prefix == "EH" {
		district = 1 + r.IntN(17)
	}
	return fmt.Sprintf("%s%d %d%c%c", prefix, district, r.IntN(10), letters[r.IntN(len(letters))], letters[r.IntN(len(letters))])
}

func registration(r *rand.Rand, year int) string {
	const letters = "ABCDEFGHJKLMNOPRSTUVWXYZ"
	l := func() byte { return letters[r.IntN(len(letters))] }
	return fmt.Sprintf("S%c%02d %c%c%c", l(), year, l(), l(), l())
}
