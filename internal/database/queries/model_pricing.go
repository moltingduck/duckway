package queries

import (
	"database/sql"
	"time"

	"github.com/hackerduck/duckway/internal/models"
)

type ModelPricingQueries struct {
	db *sql.DB
}

func NewModelPricingQueries(db *sql.DB) *ModelPricingQueries {
	return &ModelPricingQueries{db: db}
}

const modelPricingCols = `id, service_id, model, version,
	input_usd_micros_per_mtok, output_usd_micros_per_mtok,
	cache_read_usd_micros_per_mtok, cache_creation_usd_micros_per_mtok,
	reasoning_usd_micros_per_mtok,
	effective_from, created_at`

func scanModelPricing(row interface{ Scan(...interface{}) error }, p *models.ModelPricing) error {
	return row.Scan(&p.ID, &p.ServiceID, &p.Model, &p.Version,
		&p.InputUSDMicrosPerMTok, &p.OutputUSDMicrosPerMTok,
		&p.CacheReadUSDMicrosPerMTok, &p.CacheCreationUSDMicrosPerMTok,
		&p.ReasoningUSDMicrosPerMTok,
		&p.EffectiveFrom, &p.CreatedAt)
}

func (q *ModelPricingQueries) Create(p *models.ModelPricing) error {
	_, err := q.db.Exec(`INSERT INTO model_pricing
		(id, service_id, model, version, input_usd_micros_per_mtok,
		 output_usd_micros_per_mtok, cache_read_usd_micros_per_mtok,
		 cache_creation_usd_micros_per_mtok, reasoning_usd_micros_per_mtok, effective_from)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ServiceID, p.Model, p.Version, p.InputUSDMicrosPerMTok,
		p.OutputUSDMicrosPerMTok, p.CacheReadUSDMicrosPerMTok,
		p.CacheCreationUSDMicrosPerMTok, p.ReasoningUSDMicrosPerMTok, p.EffectiveFrom)
	return err
}

func (q *ModelPricingQueries) ListByService(serviceID string) ([]models.ModelPricing, error) {
	rows, err := q.db.Query(`SELECT `+modelPricingCols+` FROM model_pricing
		WHERE service_id = ? ORDER BY model, datetime(effective_from) DESC, created_at DESC`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.ModelPricing{}
	for rows.Next() {
		var p models.ModelPricing
		if err := scanModelPricing(rows, &p); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// Effective returns the newest exact-model price at the event timestamp,
// falling back to a service-wide "*" model price when configured.
func (q *ModelPricingQueries) Effective(serviceID, model string, at time.Time) (*models.ModelPricing, error) {
	var p models.ModelPricing
	err := scanModelPricing(q.db.QueryRow(`SELECT `+modelPricingCols+` FROM model_pricing
		WHERE service_id = ? AND model IN (?, '*') AND datetime(effective_from) <= datetime(?)
		ORDER BY CASE WHEN model = ? THEN 0 ELSE 1 END,
		         datetime(effective_from) DESC, created_at DESC LIMIT 1`,
		serviceID, model, at.UTC().Format(time.RFC3339Nano), model), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}
