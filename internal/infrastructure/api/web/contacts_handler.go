package web

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"iape-ui/internal/core/domain"
)

// pickerView is what the contact picker draws: the people a query may mean.
type pickerView struct {
	Query      string
	Candidates []candidateView
}

// contactPicker answers the composer's @ picker with the candidates for what was typed after the @, to show under the box.
func (a *App) contactPicker(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	view := pickerView{Query: query}
	for i, c := range domain.Candidates(query) {
		view.Candidates = append(view.Candidates, candidateView{Name: c.Name, Phone: c.Phone, Choice: strconv.Itoa(i)})
	}
	a.block(w, r, http.StatusOK, "partial:contact_picker", view)
}

// confirmCandidate confirms the candidate the user picked for what they typed, with the agent's service, and answers the mention
// to put in the message, "@(Name)", as the BFF's own page does.
func (a *App) confirmCandidate(w http.ResponseWriter, r *http.Request) {
	if !a.parseForm(w, r) {
		return
	}
	choice, ok := parseChoice(r.PostFormValue("choice"))
	if !ok {
		http.Error(w, "Choose one of the contacts.", http.StatusBadRequest)
		return
	}
	// The page says whom the user saw. The list is worked out again for the query, and if what is at that place is not who was shown
	// (they typed on, or the list changed), nobody is confirmed.
	query := r.PostFormValue("q")
	if shown := r.PostFormValue("name"); shown != "" {
		if candidates := domain.Candidates(query); choice >= len(candidates) || candidates[choice].Name != shown {
			http.Error(w, "The list changed. Choose again.", http.StatusUnprocessableEntity)
			return
		}
	}
	user, _ := UserFrom(r.Context())
	contact, err := a.svc.Interactions.ConfirmCandidate(r.Context(), user.ID, query, choice)
	if refused, ok := errors.AsType[*domain.ActionError](err); ok {
		http.Error(w, refused.Message, http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, domain.ErrAgentUnavailable) {
		http.Error(w, "The contacts service did not answer. Try again.", http.StatusBadGateway)
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "@("+contact.Name+")")
}
