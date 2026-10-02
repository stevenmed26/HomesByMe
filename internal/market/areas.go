package market

import (
	"fmt"
	"regexp"
	"strings"
)

var zipPattern = regexp.MustCompile(`^[0-9]{5}$`)
var statePattern = regexp.MustCompile(`^[A-Z]{2}$`)

func (a Area) Validate() error {
	if a.ZIP != "" && !zipPattern.MatchString(a.ZIP) {
		return fmt.Errorf("ZIP must contain exactly five digits")
	}
	if a.City != "" || a.State != "" {
		if strings.TrimSpace(a.City) == "" || !statePattern.MatchString(a.State) {
			return fmt.Errorf("city requires a nonempty city and a two-letter uppercase state")
		}
	}
	if a.ZIP == "" && strings.TrimSpace(a.City) == "" {
		return fmt.Errorf("search area requires ZIP or city/state")
	}
	return nil
}
