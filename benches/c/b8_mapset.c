/* B8 容器核 · map/set 混用压力（C 基线，-O2）。
 * 与 benches/b8_mapset.aic 逐行同构：开放寻址 + 墓碑删除 + ordpos 反向索引
 * （与 AIC runtime 的 aic_l1_map.h / aic_l1_set.h 同一套算法：used 三态
 * 0/1/2、holes 参与增长判据、order 末位交换 O(1) 删除）。 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

/* --- map[str]i64 ------------------------------------------------------------ */
typedef struct { char *key; int64_t val; } Slot;
typedef struct {
    Slot *slots; unsigned char *used; size_t *order; size_t *ordpos;
    size_t cap; size_t len; size_t holes;
} Map;

static size_t hash_str(const char *s) {
    size_t h = 1469598103934665603UL;
    for (; *s; s++) { h ^= (unsigned char)*s; h *= 1099511628211UL; }
    return h;
}

static void map_rehash(Map *m, size_t ncap) {
    size_t nn = 0;
    Slot *ns = calloc(ncap, sizeof(Slot));
    unsigned char *nu = calloc(ncap, 1);
    size_t *nop = calloc(ncap, sizeof(size_t));
    /* order 里记的是槽位，按 order 序重放（保插入序） */
    size_t norder = 0;
    size_t *no = malloc(ncap * sizeof(size_t));
    for (size_t i = 0; i < m->len; i++) {
        size_t oi = m->order[i];
        if (m->used[oi] != 1) continue;
        const char *k = m->slots[oi].key;
        size_t j = hash_str(k) & (ncap - 1);
        while (nu[j]) j = (j + 1) & (ncap - 1);
        ns[j].key = m->slots[oi].key;
        ns[j].val = m->slots[oi].val;
        nu[j] = 1;
        nop[j] = nn;
        no[nn++] = j;
    }
    free(m->slots); free(m->used); free(m->ordpos); free(m->order);
    m->slots = ns; m->used = nu; m->ordpos = nop; m->order = no;
    m->cap = ncap; m->len = nn; m->holes = 0;
    (void)norder;
}

static size_t map_find(Map *m, const char *k) {
    if (m->cap == 0) return (size_t)-1;
    size_t mask = m->cap - 1;
    size_t i = hash_str(k) & mask;
    while (m->used[i]) {
        if (m->used[i] == 1 && strcmp(m->slots[i].key, k) == 0) return i;
        i = (i + 1) & mask;
    }
    return (size_t)-1;
}

static size_t map_slot(Map *m, const char *k) {
    size_t mask = m->cap - 1;
    size_t i = hash_str(k) & mask;
    size_t first_hole = (size_t)-1;
    for (;;) {
        unsigned char u = m->used[i];
        if (u == 0) return first_hole != (size_t)-1 ? first_hole : i;
        if (u == 2) { if (first_hole == (size_t)-1) first_hole = i; }
        else if (strcmp(m->slots[i].key, k) == 0) return i;
        i = (i + 1) & mask;
    }
}

static void map_put(Map *m, const char *k, int64_t v) {
    size_t hit = map_find(m, k);
    if (hit != (size_t)-1) { m->slots[hit].val = v; return; }
    if (m->cap == 0 || (m->len + m->holes + 1) * 4 >= m->cap * 3) {
        map_rehash(m, m->cap ? m->cap * 2 : 8);
    }
    size_t i = map_slot(m, k);
    if (m->used[i] == 2) m->holes--;
    m->used[i] = 1;
    m->slots[i].key = strdup(k);
    m->slots[i].val = v;
    m->order[m->len] = i;
    m->ordpos[i] = m->len;
    m->len++;
}

static int64_t map_get(Map *m, const char *k) {
    size_t i = map_find(m, k);
    return i == (size_t)-1 ? 0 : m->slots[i].val;
}

static void map_remove(Map *m, const char *k) {
    if (m->cap == 0) return;
    size_t hit = map_find(m, k);
    if (hit == (size_t)-1) return;
    m->used[hit] = 2;
    m->holes++;
    size_t p = m->ordpos[hit];
    size_t last = m->order[--m->len];
    m->order[p] = last;
    m->ordpos[last] = p;
}

/* --- set<i64> --------------------------------------------------------------- */
typedef struct {
    int64_t *slots; unsigned char *used; size_t *order; size_t *ordpos;
    size_t cap; size_t len; size_t holes;
} Set;

static size_t hash_long(int64_t v) { return (size_t)((uint64_t)v * 11400714819323198485UL); }

static void set_rehash(Set *s, size_t ncap) {
    size_t nn = 0;
    int64_t *ns = malloc(ncap * sizeof(int64_t));
    unsigned char *nu = calloc(ncap, 1);
    size_t *nop = calloc(ncap, sizeof(size_t));
    size_t *no = malloc(ncap * sizeof(size_t));
    for (size_t i = 0; i < s->len; i++) {
        size_t oi = s->order[i];
        if (s->used[oi] != 1) continue;
        int64_t e = s->slots[oi];
        size_t j = hash_long(e) & (ncap - 1);
        while (nu[j]) j = (j + 1) & (ncap - 1);
        ns[j] = e;
        nu[j] = 1;
        nop[j] = nn;
        no[nn++] = j;
    }
    free(s->slots); free(s->used); free(s->ordpos); free(s->order);
    s->slots = ns; s->used = nu; s->ordpos = nop; s->order = no;
    s->cap = ncap; s->len = nn; s->holes = 0;
}

static size_t set_find(Set *s, int64_t v) {
    if (s->cap == 0) return (size_t)-1;
    size_t mask = s->cap - 1;
    size_t i = hash_long(v) & mask;
    while (s->used[i]) {
        if (s->used[i] == 1 && s->slots[i] == v) return i;
        i = (i + 1) & mask;
    }
    return (size_t)-1;
}

static size_t set_slot(Set *s, int64_t v) {
    size_t mask = s->cap - 1;
    size_t i = hash_long(v) & mask;
    size_t first_hole = (size_t)-1;
    for (;;) {
        unsigned char u = s->used[i];
        if (u == 0) return first_hole != (size_t)-1 ? first_hole : i;
        if (u == 2) { if (first_hole == (size_t)-1) first_hole = i; }
        else if (s->slots[i] == v) return i;
        i = (i + 1) & mask;
    }
}

static void set_add(Set *s, int64_t v) {
    if (s->cap != 0 && set_find(s, v) != (size_t)-1) return;
    if (s->cap == 0 || (s->len + s->holes + 1) * 4 >= s->cap * 3) {
        set_rehash(s, s->cap ? s->cap * 2 : 8);
    }
    size_t j = set_slot(s, v);
    if (s->used[j] == 2) s->holes--;
    s->used[j] = 1;
    s->slots[j] = v;
    s->order[s->len] = j;
    s->ordpos[j] = s->len;
    s->len++;
}

static int set_has(Set *s, int64_t v) {
    return set_find(s, v) != (size_t)-1;
}

int main(int argc, char **argv) {
    long long n = 200000;
    if (argc >= 2) n = atoll(argv[1]);
    Map m = {0};
    Set s = {0};
    char keybuf[32];
    for (long long i = 0; i < n; i++) {
        snprintf(keybuf, sizeof keybuf, "%lld", i);
        map_put(&m, keybuf, i * 3);
        set_add(&s, i);
    }
    int64_t acc = 0;
    for (long long i = 0; i < n; i++) {
        snprintf(keybuf, sizeof keybuf, "%lld", i);
        acc += map_get(&m, keybuf);
        if (set_has(&s, i)) acc += 1;
    }
    for (long long i = 0; i < n / 2; i++) {
        snprintf(keybuf, sizeof keybuf, "%lld", i);
        map_remove(&m, keybuf);
    }
    acc += (int64_t)m.len * 1000 + (int64_t)s.len;
    printf("%lld\n", (long long)acc);
    return 0;
}
