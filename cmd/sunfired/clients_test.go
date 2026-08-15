package main

import (
	"fmt"
	"sync"
	"testing"
)

// Одна машина с любым числом соединений занимает ОДНО место: иначе лимит резал
// бы нормальную работу, где на сессию приходятся десятки соединений.
func TestSeatsOneMachineManyConns(t *testing.T) {
	c := &clients{m: map[string]map[string]int{}}
	for i := 0; i < 50; i++ {
		if !c.acquire("u", "10.0.0.1", 1) {
			t.Fatalf("соединение %d отбито, а машина одна", i)
		}
	}
	if !c.acquire("u", "10.0.0.1", 1) {
		t.Fatal("та же машина не должна упираться в лимит")
	}
}

func TestSeatsLimit(t *testing.T) {
	c := &clients{m: map[string]map[string]int{}}
	if !c.acquire("u", "10.0.0.1", 2) || !c.acquire("u", "10.0.0.2", 2) {
		t.Fatal("две машины при лимите 2 должны пройти")
	}
	if c.acquire("u", "10.0.0.3", 2) {
		t.Fatal("третья машина при лимите 2 должна быть отбита")
	}
	// место освободилось — третья проходит
	c.release("u", "10.0.0.1", 2)
	if !c.acquire("u", "10.0.0.3", 2) {
		t.Fatal("после освобождения места третья машина должна пройти")
	}
}

func TestSeatsNoLimit(t *testing.T) {
	c := &clients{m: map[string]map[string]int{}}
	for i := 0; i < 200; i++ {
		if !c.acquire("u", fmt.Sprintf("10.0.%d.%d", i/256, i%256), 0) {
			t.Fatal("лимит 0 не должен ограничивать")
		}
	}
}

// Карта не должна расти: после освобождения всех мест записей не остаётся.
func TestSeatsNoLeak(t *testing.T) {
	c := &clients{m: map[string]map[string]int{}}
	for i := 0; i < 10; i++ {
		c.acquire("u", "10.0.0.1", 5)
	}
	for i := 0; i < 10; i++ {
		c.release("u", "10.0.0.1", 5)
	}
	if len(c.m) != 0 {
		t.Fatalf("в карте осталось %d записей", len(c.m))
	}
}

func TestSeatsParallel(t *testing.T) {
	c := &clients{m: map[string]map[string]int{}}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.0.%d", i%8)
			for j := 0; j < 100; j++ {
				if c.acquire("u", ip, 8) {
					c.release("u", ip, 8)
				}
			}
		}(i)
	}
	wg.Wait()
	if len(c.m) != 0 {
		t.Fatalf("после параллельной работы осталось %d записей", len(c.m))
	}
}
