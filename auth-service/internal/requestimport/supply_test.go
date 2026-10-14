package requestimport

import (
	"encoding/json"
	"testing"
)

func TestFallbackSupplyReadsTemplateColumns(t *testing.T) {
	document := FallbackSupply("Накладная № П-777 от 14.10.2026\nПродукт;Количество;Срок хранения (ч);Единица (г / мл / шт)\nМоцарелла;5;120;кг\nСливки 33 %;6000;96;мл\nЯйцо куриное;180;600;шт")
	want := []SupplyItem{
		{ProductName: "Моцарелла", Quantity: 5, Unit: "kg", ShelfLifeHours: 120},
		{ProductName: "Сливки 33 %", Quantity: 6000, Unit: "ml", ShelfLifeHours: 96},
		{ProductName: "Яйцо куриное", Quantity: 180, Unit: "pcs", ShelfLifeHours: 600},
	}
	if document.SupplyNumber != "П-777" {
		t.Fatalf("expected supply number П-777, got %q", document.SupplyNumber)
	}
	if len(document.Items) != len(want) {
		t.Fatalf("expected %d items, got %d: %#v", len(want), len(document.Items), document.Items)
	}
	for i := range want {
		got := document.Items[i]
		if got.ProductName != want[i].ProductName || got.Quantity != want[i].Quantity || got.Unit != want[i].Unit || got.ShelfLifeHours != want[i].ShelfLifeHours {
			t.Fatalf("item %d: got %#v, want %#v", i, got, want[i])
		}
	}
}

func TestFallbackSupplyWithoutShelfLife(t *testing.T) {
	document := FallbackSupply("Молоко | 2 л\nсахар 500 грамм")
	if len(document.Items) != 2 {
		t.Fatalf("expected two items, got %d: %#v", len(document.Items), document.Items)
	}
	if document.Items[0].ProductName != "Молоко" || document.Items[0].Quantity != 2 || document.Items[0].Unit != "l" || document.Items[0].ShelfLifeHours != 0 {
		t.Fatalf("unexpected first item: %#v", document.Items[0])
	}
	if document.Items[1].ProductName != "сахар" || document.Items[1].Quantity != 500 || document.Items[1].Unit != "g" {
		t.Fatalf("unexpected second item: %#v", document.Items[1])
	}
}

func TestInsightListsAcceptObjects(t *testing.T) {
	var insight Insight
	raw := `{"summary":"ok","new_dishes":[{"name":"Оливье","есть":"яйца","нет":"горошек"},"Глинтвейн"],"menu_picks":"Сырники — к чаю"}`
	if err := json.Unmarshal([]byte(raw), &insight); err != nil {
		t.Fatal(err)
	}
	if len(insight.NewDishes) != 2 || insight.NewDishes[0] != "Оливье — есть: яйца — нет: горошек" || insight.NewDishes[1] != "Глинтвейн" {
		t.Fatalf("unexpected new dishes: %#v", insight.NewDishes)
	}
	if len(insight.MenuPicks) != 1 || insight.MenuPicks[0] != "Сырники — к чаю" {
		t.Fatalf("unexpected menu picks: %#v", insight.MenuPicks)
	}
}
