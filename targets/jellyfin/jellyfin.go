package jellyfin

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/cloudbox/autoscan"
)

// Config rozszerzone o:
// - UserID: ID użytkownika Jellyfin do zapytań /Users/{userId}/...
// - Library: nazwa biblioteki (np. "Filmy") – używana do pobrania ViewID
// - PreciseRefresh: jeśli true, zamiast pełnego skanu biblioteki
//   odświeżamy konkretny element (folder/film) po jego itemId.
type Config struct {
	URL            string             `yaml:"url"`
	Token          string             `yaml:"token"`
	UserID         string             `yaml:"user_id"`        // NOWE
	Library        string             `yaml:"library"`        // NOWE (opcjonalne; jeśli puste, wybieramy na podstawie ścieżki)
	PreciseRefresh bool               `yaml:"precise_refresh"`// NOWE
	Rewrite        []autoscan.Rewrite `yaml:"rewrite"`
	Verbosity      string             `yaml:"verbosity"`
}

// target przechowuje bieżącą konfigurację i klienta API.
// Trzymamy całe Config, aby mieć dostęp do UserID/Library/PreciseRefresh.
type target struct {
	cfg Config

	libraries []library

	log     zerolog.Logger
	rewrite autoscan.Rewriter
	api     apiClient
}

func New(c Config) (autoscan.Target, error) {
	l := autoscan.GetLogger(c.Verbosity).With().
		Str("target", "jellyfin").
		Str("url", c.URL).
		Logger()

	rewriter, err := autoscan.NewRewriter(c.Rewrite)
	if err != nil {
		return nil, err
	}

	api := newAPIClient(c.URL, c.Token, l)

	libraries, err := api.Libraries()
	if err != nil {
		return nil, err
	}

	l.Debug().
		Interface("libraries", libraries).
		Msg("Retrieved libraries")

	return &target{
		cfg: c,

		libraries: libraries,
		log:       l,
		rewrite:   rewriter,
		api:       api,
	}, nil
}

func (t target) Available() error {
	return t.api.Available()
}

func (t target) Scan(scan autoscan.Scan) error {
	// Przepisz ścieżkę według rewrite (perspektywa Jellyfin).
	scanFolder := t.rewrite(scan.Folder)

	// Ustal bibliotekę na podstawie ścieżki.
	lib, err := t.getScanLibrary(scanFolder)
	if err != nil {
		t.log.Warn().
			Err(err).
			Msg("No target libraries found")
		return nil
	}

	l := t.log.With().
		Str("path", scanFolder).
		Str("library", lib.Name).
		Logger()

	// Jeśli włączony precyzyjny refresh – najpierw spróbuj odświeżyć
	// tylko wskazany element po jego itemId (dokładne dopasowanie Path).
	if t.cfg.PreciseRefresh {
		l.Trace().Msg("Trying precise Jellyfin refresh by itemId")

		// Ustal ViewID biblioteki: jeśli w configu podano Library, użyj jej,
		// w przeciwnym razie bierz nazwę biblioteki z dopasowania ścieżki.
		libraryName := t.cfg.Library
		if strings.TrimSpace(libraryName) == "" {
			libraryName = lib.Name
		}

		viewID, vErr := t.api.GetViewID(t.cfg.UserID, libraryName)
		if vErr != nil {
			l.Warn().Err(vErr).Str("library", libraryName).
				Msg("Cannot resolve Jellyfin viewId; falling back to library scan")
		} else {
			itemID, fErr := t.api.FindItemIDByPath(t.cfg.UserID, viewID, scanFolder)
			if fErr != nil {
				l.Warn().Err(fErr).Str("path", scanFolder).
					Msg("Cannot match Jellyfin item by exact Path; falling back to library scan")
			} else if strings.TrimSpace(itemID) != "" {
				// Odśwież tylko ten element (rekurencyjnie).
				if rErr := t.api.RefreshItem(itemID); rErr != nil {
					l.Error().Err(rErr).Str("itemId", itemID).
						Msg("Jellyfin item refresh failed; falling back to library scan")
				} else {
					l.Info().Str("itemId", itemID).
						Msg("Refreshed Jellyfin item recursively (precise refresh)")
					return nil
				}
			}
		}
	}

	// Fallback lub tryb klasyczny: wyślij standardowy skan (cała biblioteka).
	l.Trace().Msg("Sending library scan request (fallback or precise_refresh disabled)")
	if err := t.api.Scan(scanFolder); err != nil {
		return err
	}
	l.Info().Msg("Scan moved to target")
	return nil
}

// getScanLibrary zwraca bibliotekę, do której należy ścieżka (po rewrite).
func (t target) getScanLibrary(folder string) (*library, error) {
	for _, l := range t.libraries {
		if strings.HasPrefix(folder, l.Path) {
			return &l, nil
		}
	}
	return nil, fmt.Errorf("%v: failed determining library", folder)
}
