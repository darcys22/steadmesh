package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/jackc/pgx/v5"
)

// SkillSelection includes source provenance. A registry key reused for a new
// repository or revision never authorizes the old runtime choice implicitly.
type SkillSelection struct {
	Ref    spec.SkillRef         `json:"ref"`
	Source spec.SkillMarketplace `json:"source"`
	Owner  string                `json:"owner"`
}

type SkillsSnapshot struct {
	Manifest   compile.SeatManifest
	Selections []SkillSelection
}

// Fingerprint binds an inspected/size-validated set to the exact policy and
// effective selections. Compare under the write lock before accepting a choice.
func (s SkillsSnapshot) Fingerprint() string {
	raw, _ := json.Marshal(struct {
		Policy   *spec.SkillPolicy
		Sources  map[string]spec.SkillMarketplace
		Selected []SkillSelection
	}{s.Manifest.Skills, s.Manifest.SkillMarketplaces, s.Selections})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func SkillSourceFingerprint(source spec.SkillMarketplace) string {
	b, _ := json.Marshal(source)
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func SkillMode(sm *compile.SeatManifest) string {
	if sm.Skills == nil {
		return ""
	}
	if sm.Skills.Mode == "" {
		return "explicit"
	}
	return sm.Skills.Mode
}

func skillLimit(sm *compile.SeatManifest) int {
	if sm.Skills == nil {
		return 0
	}
	n := sm.Skills.MaxDynamicSkills
	if n == 0 {
		n = 16
	}
	if n < 0 || n > 128 {
		return 0
	}
	return int(n)
}

var safeSkillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

func ValidSkillRef(ref spec.SkillRef) bool {
	return safeSkillName.MatchString(ref.Marketplace) && safeSkillName.MatchString(ref.Plugin) && safeSkillName.MatchString(ref.Skill)
}

// AuthorizedSkillSource always derives source identity from committed policy.
func AuthorizedSkillSource(sm *compile.SeatManifest, ref spec.SkillRef) (spec.SkillMarketplace, error) {
	mode := SkillMode(sm)
	if !ValidSkillRef(ref) || (mode != "explicit" && mode != "discover" && mode != "agent_choice") || !slices.Contains(sm.Skills.Marketplaces, ref.Marketplace) {
		return spec.SkillMarketplace{}, fmt.Errorf("%w: skill is not allowed by current policy", ErrInvalid)
	}
	source, ok := sm.SkillMarketplaces[ref.Marketplace]
	if !ok || source.Repository == "" || source.Ref == "" {
		return source, fmt.Errorf("%w: skill marketplace is not configured", ErrInvalid)
	}
	return source, nil
}

func terraformSkill(sm *compile.SeatManifest, ref spec.SkillRef) bool {
	return sm.Skills != nil && slices.Contains(sm.Skills.Install, ref)
}

func skillRefKey(ref spec.SkillRef) string {
	return ref.Marketplace + "/" + ref.Plugin + "/" + ref.Skill
}

// effectiveSkills is defensive even if the declaration bypassed validation.
// Explicit invalid references fail closed, while no-longer-authorized runtime
// choices are excluded, independently of the asynchronous runtime refresh.
func effectiveSkills(sm *compile.SeatManifest, dynamic []SkillSelection) ([]SkillSelection, error) {
	out := []SkillSelection{}
	if sm.Skills == nil {
		return out, nil
	}
	mode := SkillMode(sm)
	if mode != "explicit" && mode != "discover" && mode != "agent_choice" {
		return nil, fmt.Errorf("%w: invalid skills mode", ErrInvalid)
	}
	seen := map[spec.SkillRef]bool{}
	for _, ref := range sm.Skills.Install {
		source, err := AuthorizedSkillSource(sm, ref)
		if err != nil {
			return nil, err
		}
		if !seen[ref] {
			out = append(out, SkillSelection{Ref: ref, Source: source, Owner: "terraform"})
			seen[ref] = true
		}
	}
	if mode == "agent_choice" {
		sort.Slice(dynamic, func(i, j int) bool { return skillRefKey(dynamic[i].Ref) < skillRefKey(dynamic[j].Ref) })
		n := 0
		for _, selected := range dynamic {
			source, err := AuthorizedSkillSource(sm, selected.Ref)
			if err != nil || source != selected.Source || seen[selected.Ref] || n >= skillLimit(sm) {
				continue
			}
			selected.Owner = "agent"
			out = append(out, selected)
			seen[selected.Ref] = true
			n++
		}
	}
	if len(out) > runtimeapi.MaxSkillBundles {
		return nil, fmt.Errorf("%w: too many selected skills", ErrInvalid)
	}
	sort.Slice(out, func(i, j int) bool { return skillRefKey(out[i].Ref) < skillRefKey(out[j].Ref) })
	return out, nil
}

func selectedSkills(ctx context.Context, tx pgx.Tx, seatID string) ([]SkillSelection, error) {
	rows, err := tx.Query(ctx, `SELECT marketplace,plugin,skill,repository,source_ref,source_fingerprint FROM seat_skill_selections WHERE seat_id=$1 ORDER BY marketplace,plugin,skill`, seatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillSelection{}
	for rows.Next() {
		var v SkillSelection
		var fingerprint string
		if err := rows.Scan(&v.Ref.Marketplace, &v.Ref.Plugin, &v.Ref.Skill, &v.Source.Repository, &v.Source.Ref, &fingerprint); err != nil {
			return nil, err
		}
		if fingerprint != SkillSourceFingerprint(v.Source) {
			return nil, fmt.Errorf("%w: skill source fingerprint mismatch", ErrInvalid)
		}
		v.Owner = "agent"
		out = append(out, v)
	}
	return out, rows.Err()
}

// SkillsSnapshot reads current policy and choices in one coherent snapshot.
func (s *Store) SkillsSnapshot(ctx context.Context, orgID, seatID string) (SkillsSnapshot, error) {
	var out SkillsSnapshot
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT s.manifest FROM seats s JOIN organizations o ON o.id=s.organization_id WHERE s.id=$1 AND s.organization_id=$2 AND s.retired_at IS NULL AND o.deleted_at IS NULL`, seatID, orgID).Scan(&raw); err != nil {
			return notFound(err)
		}
		if err := json.Unmarshal(raw, &out.Manifest); err != nil {
			return err
		}
		dynamic, err := selectedSkills(ctx, tx, seatID)
		if err != nil {
			return err
		}
		out.Selections, err = effectiveSkills(&out.Manifest, dynamic)
		return err
	})
	return out, err
}

// ChangeSkillSelection serializes policy changes and concurrent installs on
// organization then seat, like SyncOrganization. The source was resolved before
// the transaction; verify it against the latest committed manifest before write.
// expectedFingerprint must bind the exact snapshot whose candidate union was
// checked for aggregate size and file limits; concurrent changes require retry.
func (s *Store) ChangeSkillSelection(ctx context.Context, f Fence, orgID string, ref spec.SkillRef, resolvedSource spec.SkillMarketplace, install bool, expectedFingerprint string) error {
	if !ValidSkillRef(ref) {
		return fmt.Errorf("%w: invalid skill reference", ErrInvalid)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, orgID).Scan(&id); err != nil {
			return notFound(err)
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT manifest FROM seats WHERE id=$1 AND organization_id=$2 AND retired_at IS NULL AND retiring_at IS NULL FOR UPDATE`, f.SeatID, orgID).Scan(&raw); err != nil {
			return notFound(err)
		}
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var sm compile.SeatManifest
		if err := json.Unmarshal(raw, &sm); err != nil {
			return err
		}
		if SkillMode(&sm) != "agent_choice" {
			return fmt.Errorf("%w: dynamic skill choices are disabled", ErrInvalid)
		}
		source, err := AuthorizedSkillSource(&sm, ref)
		if err != nil {
			return err
		}
		if source != resolvedSource {
			return fmt.Errorf("%w: skill source changed; inspect the current marketplace again", ErrConflict)
		}
		currentChoices, err := selectedSkills(ctx, tx, f.SeatID)
		if err != nil {
			return err
		}
		effective, err := effectiveSkills(&sm, currentChoices)
		if err != nil {
			return err
		}
		current := SkillsSnapshot{Manifest: sm, Selections: effective}
		if expectedFingerprint != current.Fingerprint() {
			return fmt.Errorf("%w: skills policy or selections changed; inspect and retry", ErrConflict)
		}
		if terraformSkill(&sm, ref) {
			if install {
				return nil
			}
			return fmt.Errorf("%w: Terraform-owned skills cannot be removed by an agent", ErrInvalid)
		}
		if err := pruneSkillSelections(ctx, tx, f.SeatID, &sm); err != nil {
			return err
		}
		if !install {
			_, err := tx.Exec(ctx, `DELETE FROM seat_skill_selections WHERE seat_id=$1 AND marketplace=$2 AND plugin=$3 AND skill=$4`, f.SeatID, ref.Marketplace, ref.Plugin, ref.Skill)
			return err
		}
		selected, err := selectedSkills(ctx, tx, f.SeatID)
		if err != nil {
			return err
		}
		for _, v := range selected {
			if v.Ref == ref {
				return nil
			}
		}
		if len(selected) >= skillLimit(&sm) {
			return fmt.Errorf("%w: maximum dynamic skill count reached", ErrInvalid)
		}
		_, err = tx.Exec(ctx, `INSERT INTO seat_skill_selections (seat_id,marketplace,plugin,skill,repository,source_ref,source_fingerprint) VALUES ($1,$2,$3,$4,$5,$6,$7)`, f.SeatID, ref.Marketplace, ref.Plugin, ref.Skill, source.Repository, source.Ref, SkillSourceFingerprint(source))
		return err
	})
}

// Called inside apply's transaction: unchanged declarations preserve runtime
// choices, but revocation cannot be undone by a rapid remove/re-add of a source.
func pruneSkillSelections(ctx context.Context, tx pgx.Tx, seatID string, sm *compile.SeatManifest) error {
	selected, err := selectedSkills(ctx, tx, seatID)
	if err != nil {
		return err
	}
	effective, err := effectiveSkills(sm, selected)
	if err != nil {
		return err
	}
	keep := map[spec.SkillRef]bool{}
	for _, v := range effective {
		if v.Owner == "agent" {
			keep[v.Ref] = true
		}
	}
	for _, v := range selected {
		if !keep[v.Ref] {
			if _, err := tx.Exec(ctx, `DELETE FROM seat_skill_selections WHERE seat_id=$1 AND marketplace=$2 AND plugin=$3 AND skill=$4`, seatID, v.Ref.Marketplace, v.Ref.Plugin, v.Ref.Skill); err != nil {
				return err
			}
		}
	}
	return nil
}
