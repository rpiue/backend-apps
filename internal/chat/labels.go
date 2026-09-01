package chat

import (
	"context"
	"strings"
)

// Label es una etiqueta de conversación (organización del operador).
type Label struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Color  string `json:"color"`
	Notify bool   `json:"notify"`
}

// listLabels devuelve todas las etiquetas ordenadas por nombre.
func (d *DB) listLabels(ctx context.Context) ([]Label, error) {
	rows, err := d.pool.Query(ctx, `select id, name, color, notify from chat_labels order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.Color, &l.Notify); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// createLabel crea (o devuelve si ya existe por nombre) una etiqueta.
func (d *DB) createLabel(ctx context.Context, name, color string) (*Label, error) {
	name = strings.TrimSpace(name)
	if color == "" {
		color = "#008069"
	}
	var l Label
	err := d.pool.QueryRow(ctx, `
		insert into chat_labels (name, color) values ($1,$2)
		on conflict (name) do update set color = excluded.color
		returning id, name, color, notify`, name, color).Scan(&l.ID, &l.Name, &l.Color, &l.Notify)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// updateLabel actualiza nombre/color/notify de una etiqueta.
func (d *DB) updateLabel(ctx context.Context, id int64, name, color string, notify bool) (*Label, error) {
	var l Label
	err := d.pool.QueryRow(ctx, `
		update chat_labels set name = $2, color = $3, notify = $4 where id = $1
		returning id, name, color, notify`,
		id, strings.TrimSpace(name), color, notify).Scan(&l.ID, &l.Name, &l.Color, &l.Notify)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// deleteLabel elimina una etiqueta (y sus asignaciones por cascade).
func (d *DB) deleteLabel(ctx context.Context, id int64) error {
	_, err := d.pool.Exec(ctx, `delete from chat_labels where id = $1`, id)
	return err
}

// assignLabel asigna una etiqueta a una conversación (idempotente).
func (d *DB) assignLabel(ctx context.Context, conversationID, labelID int64) error {
	_, err := d.pool.Exec(ctx, `
		insert into chat_conversation_labels (conversation_id, label_id)
		values ($1,$2) on conflict do nothing`, conversationID, labelID)
	return err
}

// unassignLabel quita una etiqueta de una conversación.
func (d *DB) unassignLabel(ctx context.Context, conversationID, labelID int64) error {
	_, err := d.pool.Exec(ctx,
		`delete from chat_conversation_labels where conversation_id = $1 and label_id = $2`,
		conversationID, labelID)
	return err
}

// labelsForConversationIDs devuelve, por cada conversación pedida, sus etiquetas.
// Se usa para adjuntarlas "frescas" fuera de la consulta cacheada de la bandeja.
func (d *DB) labelsForConversationIDs(ctx context.Context, ids []int64) (map[int64][]Label, error) {
	out := map[int64][]Label{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		select cl.conversation_id, l.id, l.name, l.color, l.notify
		from chat_conversation_labels cl
		join chat_labels l on l.id = cl.label_id
		where cl.conversation_id = any($1)
		order by l.name`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int64
		var l Label
		if err := rows.Scan(&cid, &l.ID, &l.Name, &l.Color, &l.Notify); err != nil {
			return nil, err
		}
		out[cid] = append(out[cid], l)
	}
	return out, rows.Err()
}
