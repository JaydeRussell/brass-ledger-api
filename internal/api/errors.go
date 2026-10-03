package api

import (
	"log"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// The frontend shows a response's "error" text to the user as-is, so
// these say what happened in plain words. The underlying error, which
// can carry database details or BCP URLs with player ids in them, goes
// to the log.
const (
	internalErrorMessage  = "Something went wrong on our side. Please try again."
	bcpUnavailableMessage = "Couldn't reach Best Coast Pairings just now. Please try again in a moment."
	bcpGoneMessage        = "Best Coast Pairings has no record of this. It may have been deleted."
)

// internalError logs err against the route and answers 500 with a
// generic message.
func internalError(c echo.Context, err error) error {
	log.Printf("%s %s: %v", c.Request().Method, c.Path(), err)
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": internalErrorMessage})
}

// bcpError logs a failed BCP call and answers 502 with a message that
// distinguishes "doesn't exist" from "try again".
func bcpError(c echo.Context, err error) error {
	log.Printf("%s %s: BCP: %v", c.Request().Method, c.Path(), err)
	msg := bcpUnavailableMessage
	if bcp.IsGone(err) {
		msg = bcpGoneMessage
	}
	return c.JSON(http.StatusBadGateway, map[string]string{"error": msg})
}
