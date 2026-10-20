package tablerefs

import (
	"reflect"
	"strings"
	"testing"
)

// Every case is copied verbatim from esql_test.go's TestIndexToTableIDRefs; when
// the backend grows a case, copy it down.
func TestCanonicalizeMirrorsTheServer(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		inputModels    []string
		wantSQL        string
		wantUnresolved bool
	}{
		{
			name:        "single numeric ref de-normalized to real id",
			sql:         "SELECT * FROM {{ ref('0') }}",
			inputModels: []string{"table-abc"},
			wantSQL:     "SELECT * FROM {{ ref('table-abc') }}",
		},
		{
			name:        "multiple numeric refs de-normalized in order",
			sql:         "SELECT * FROM {{ ref('0') }} o JOIN {{ ref('1') }} c ON o.cid = c.id",
			inputModels: []string{"table-orders", "table-customers"},
			wantSQL:     "SELECT * FROM {{ ref('table-orders') }} o JOIN {{ ref('table-customers') }} c ON o.cid = c.id",
		},
		{
			name:        "real-ID ref left unchanged (no-op for new rows)",
			sql:         "SELECT * FROM {{ ref('table-abc') }}",
			inputModels: []string{"table-abc"},
			wantSQL:     "SELECT * FROM {{ ref('table-abc') }}",
		},
		{
			name:        "legacy modelv2- prefix counts as real",
			sql:         "SELECT * FROM {{ ref('0') }}",
			inputModels: []string{"modelv2-legacy"},
			wantSQL:     "SELECT * FROM {{ ref('modelv2-legacy') }}",
		},
		{
			name:           "out-of-range index left unchanged and flagged",
			sql:            "SELECT * FROM {{ ref('5') }}",
			inputModels:    []string{"table-abc"},
			wantSQL:        "SELECT * FROM {{ ref('5') }}",
			wantUnresolved: true,
		},
		{
			name:           "corrupt row — positional target is itself non-real",
			sql:            "SELECT * FROM {{ ref('0') }}",
			inputModels:    []string{"0", "table-abc"},
			wantSQL:        "SELECT * FROM {{ ref('0') }}",
			wantUnresolved: true,
		},
		{
			name:        "no refs — passthrough",
			sql:         "SELECT 1",
			inputModels: []string{"table-abc"},
			wantSQL:     "SELECT 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unresolved := Canonicalize(tt.sql, tt.inputModels)
			if got != tt.wantSQL {
				t.Errorf("SQL mismatch\ngot:  %s\nwant: %s", got, tt.wantSQL)
			}
			if unresolved != tt.wantUnresolved {
				t.Errorf("unresolved mismatch: got %v want %v", unresolved, tt.wantUnresolved)
			}
		})
	}
}

// Pins the parts of the copied pattern that look like typos: doubled braces,
// either quote style, whitespace inside the call.
func TestCanonicalizeGrammarTolerance(t *testing.T) {
	inputs := []string{"table-orders"}
	for _, tt := range []struct {
		name string
		sql  string
		want string
	}{
		{"double quotes", `SELECT * FROM {{ ref("0") }}`, `SELECT * FROM {{ ref('table-orders') }}`},
		{"no inner spaces", "SELECT * FROM {{ref('0')}}", "SELECT * FROM {{ ref('table-orders') }}"},
		{"generous spacing", "SELECT * FROM {{   ref(  '0'  )   }}", "SELECT * FROM {{ ref('table-orders') }}"},
		{"f-string over-escaping", "SELECT * FROM {{{{ ref('0') }}}}", "SELECT * FROM {{ ref('table-orders') }}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, unresolved := Canonicalize(tt.sql, inputs)
			if got != tt.want {
				t.Errorf("got:  %s\nwant: %s", got, tt.want)
			}
			if unresolved {
				t.Error("nothing here is unresolvable")
			}
		})
	}
}

// aiBuiltCode is the positional-ref shape older stored rows hold, the case
// canonicalization exists for.
const aiBuiltCode = `WITH monthly AS (
    SELECT
        date_trunc('month', o.order_date) AS month,
        o.customer_id,
        SUM(o.total_amount) AS revenue
    FROM {{ ref('0') }} o
    WHERE o.status = 'completed'
    GROUP BY 1, 2
)
SELECT
    m.month,
    c.segment,
    COUNT(DISTINCT m.customer_id) AS customers,
    SUM(m.revenue) AS revenue,
    SUM(m.revenue) / NULLIF(COUNT(DISTINCT m.customer_id), 0) AS revenue_per_customer
FROM monthly m
JOIN {{ ref('1') }} c ON c.id = m.customer_id
LEFT JOIN {{ ref('2') }} r ON r.customer_id = m.customer_id AND r.month = m.month
WHERE c.segment IS NOT NULL
GROUP BY 1, 2
ORDER BY 1 DESC, 4 DESC`

func TestCanonicalizeRealisticAIBuiltCode(t *testing.T) {
	inputs := []string{"table-orders", "table-customers", "table-refunds"}
	got, unresolved := Canonicalize(aiBuiltCode, inputs)
	if unresolved {
		t.Fatal("every ref resolves here")
	}
	for _, want := range []string{
		"{{ ref('table-orders') }}", "{{ ref('table-customers') }}", "{{ ref('table-refunds') }}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ref('0')") || strings.Contains(got, "ref('1')") || strings.Contains(got, "ref('2')") {
		t.Errorf("a positional ref survived:\n%s", got)
	}
	if !strings.Contains(got, "SUM(m.revenue) / NULLIF(COUNT(DISTINCT m.customer_id), 0)") {
		t.Errorf("the SQL body was disturbed:\n%s", got)
	}
}

// The sync baseline hashes the canonical form, so these two properties are what
// make a local hash comparable to a remote one.
func TestCanonicalizeIsIdempotentAndByteExactOnIDForm(t *testing.T) {
	inputs := []string{"table-orders", "table-customers", "table-refunds"}
	once, _ := Canonicalize(aiBuiltCode, inputs)
	twice, unresolved := Canonicalize(once, inputs)
	if twice != once {
		t.Errorf("not idempotent:\ngot:  %s\nwant: %s", twice, once)
	}
	if unresolved {
		t.Error("id-form code has nothing unresolvable in it")
	}

	// Any reformat of hand-written id-form code would be permanent phantom drift.
	handWritten := "SELECT *\nFROM {{ref(\"table-orders\")}}   -- note the spelling\nWHERE 1 = 1\n"
	got, _ := Canonicalize(handWritten, inputs)
	if got != handWritten {
		t.Errorf("id-form code must be byte-identical\ngot:  %q\nwant: %q", got, handWritten)
	}
}

func TestDeriveInputModels(t *testing.T) {
	for _, tt := range []struct {
		name string
		code string
		want []string
	}{
		{"none", "SELECT 1", nil},
		{
			name: "sorted, not first-seen",
			code: "SELECT * FROM {{ ref('table-orders') }} o JOIN {{ ref('table-customers') }} c ON 1=1",
			want: []string{"table-customers", "table-orders"},
		},
		{
			name: "self-join de-duplicates",
			code: "FROM {{ ref('table-x') }} a JOIN {{ ref('table-x') }} b ON a.id = b.id",
			want: []string{"table-x"},
		},
		{
			name: "legacy prefix counts",
			code: "SELECT * FROM {{ ref('modelv2-legacy') }}",
			want: []string{"modelv2-legacy"},
		},
		{
			name: "a positional ref is NOT an input",
			code: "SELECT * FROM {{ ref('0') }} JOIN {{ ref('table-real') }} ON 1=1",
			want: []string{"table-real"},
		},
		{
			name: "a non-id string is NOT an input",
			code: "SELECT * FROM {{ ref('./staging_orders.sql') }}",
			want: nil,
		},
		{
			name: "grammar tolerance matches Canonicalize's",
			code: `FROM {{{{ ref("table-b") }}}} JOIN {{ref('table-a')}} ON 1=1`,
			want: []string{"table-a", "table-b"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveInputModels(tt.code)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v want %v", got, tt.want)
			}
		})
	}
}

// After canonicalization, every ref is an input and every input a ref; push
// sends both on one write, so a disagreement is lineage that does not match the
// SQL.
func TestDeriveInputModelsAgreesWithCanonicalize(t *testing.T) {
	inputs := []string{"table-orders", "table-customers", "table-refunds"}
	canonical, _ := Canonicalize(aiBuiltCode, inputs)
	got := DeriveInputModels(canonical)
	want := []string{"table-customers", "table-orders", "table-refunds"} // sorted
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// Positional code yields nothing, which is why remote code is canonicalized
	// first.
	if raw := DeriveInputModels(aiBuiltCode); raw != nil {
		t.Errorf("positional code declares no ids, got %v", raw)
	}
}

func TestIsTableID(t *testing.T) {
	for _, id := range []string{"table-abc", "modelv2-legacy"} {
		if !IsTableID(id) {
			t.Errorf("%q is a table id", id)
		}
	}
	for _, id := range []string{"", "0", "collection-1", "wf-1", "tables-abc", "./x.sql"} {
		if IsTableID(id) {
			t.Errorf("%q is not a table id", id)
		}
	}
}
