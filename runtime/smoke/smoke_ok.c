/* AIC runtime 冒烟测试（非 gate；R3 emit 交付后由 H2/H3/H4 golden 接管）。
 * 覆盖 L0 区域/守卫/浮点/str + L1 容器；输出确定，供四配置逐位比对。 */
#include "aic_l1.h"
#include "aic_std.h"

#include <stdio.h>

typedef struct { aic_hdr hdr; aic_i32 v; } smoke_obj;

int main(void) {
    aic_task_init("<smoke>", 1);
    aic_os_init(0, NULL);

    /* --- list：增长、插入、删除、索引读、pop --- */
    aic_list_i32 *xs = aic_list_new_i32(2);
    aic_list_append_i32(xs, 10, "<>", 2);
    aic_list_append_i32(xs, 20, "<>", 2);
    aic_list_append_i32(xs, 30, "<>", 2);
    aic_list_insert_i32(xs, 1, 15, "<>", 2);
    aic_list_remove_i32(xs, 0, "<>", 2);
    printf("list len=%llu", (unsigned long long)aic_list_len_i32(xs));
    for (aic_usize i = 0; i < aic_list_len_i32(xs); i++) {
        printf(" %d", aic_list_get_i32(xs, i, "<>", 2));
    }
    printf("\n");
    bool ok = false;
    aic_i32 last = aic_list_pop_i32(xs, &ok);
    printf("pop ok=%d v=%d len=%llu\n", ok ? 1 : 0, last,
           (unsigned long long)aic_list_len_i32(xs));

    /* 零值容器读作空 */
    aic_list_i32 *nill = NULL;
    printf("nil len=%llu isempty=%d\n", (unsigned long long)aic_list_len_i32(nill),
           aic_list_isempty_i32(nill) ? 1 : 0);

    /* --- map：覆盖保位、未命中零值、插入序 --- */
    aic_map_str_i32 *m = aic_map_new_str_i32(3);
    aic_map_put_str_i32(m, (aic_str){ "a", 1 }, 1, "<>", 3);
    aic_map_put_str_i32(m, (aic_str){ "b", 1 }, 2, "<>", 3);
    aic_map_put_str_i32(m, (aic_str){ "a", 1 }, 9, "<>", 3);
    printf("map len=%llu a=%d miss=%d key0=%.*s\n",
           (unsigned long long)aic_map_len_str_i32(m),
           aic_map_get_str_i32(m, (aic_str){ "a", 1 }),
           aic_map_get_str_i32(m, (aic_str){ "z", 1 }),
           (int)aic_map_key_at_str_i32(m, 0, "<>", 3).len,
           aic_map_key_at_str_i32(m, 0, "<>", 3).p);

    /* --- set：去重、插入序、删除 --- */
    aic_set_i32 *s = aic_set_new_i32(4);
    aic_set_add_i32(s, 5, "<>", 4);
    aic_set_add_i32(s, 5, "<>", 4);
    aic_set_add_i32(s, 7, "<>", 4);
    aic_set_remove_i32(s, 5, "<>", 4);
    aic_set_add_i32(s, 5, "<>", 4);
    printf("set len=%llu at0=%d has5=%d has7=%d\n",
           (unsigned long long)aic_set_len_i32(s),
           aic_set_at_i32(s, 0, "<>", 4), aic_set_has_i32(s, 5) ? 1 : 0,
           aic_set_has_i32(s, 7) ? 1 : 0);

    /* --- str：concat / split / trim / sub / indexOf --- */
    aic_str hi = aic_str_concat((aic_str){ "Hello, ", 7 }, (aic_str){ "AIC", 3 }, 5);
    printf("concat=%.*s indexOf=%llu\n", (int)hi.len, hi.p,
           (unsigned long long)aic_str_index_of(hi, (aic_str){ "AIC", 3 }));
    aic_list_str *parts = aic_std_str_split((aic_str){ "a,b,c", 5 }, (aic_str){ ",", 1 }, "<>", 6);
    printf("split len=%llu p1=%.*s\n", (unsigned long long)aic_list_len_str(parts),
           (int)aic_list_get_str(parts, 1, "<>", 6).len,
           aic_list_get_str(parts, 1, "<>", 6).p);
    aic_str tv = aic_str_trim((aic_str){ "  x  ", 5 });
    printf("trim=[%.*s] sub=[%.*s]\n", (int)tv.len, tv.p,
           (int)aic_str_sub(tv, 0, 1, "<>", 6).len, aic_str_sub(tv, 0, 1, "<>", 6).p);

    /* --- f64 最短往返 --- */
    aic_f64 vals[7] = { 0.1, 1.0 / 3.0, 1e300, -0.0, 3.14159, 123456789.0, 2.5 };
    for (int i = 0; i < 7; i++) {
        char b[40];
        aic_usize n = aic_f64_fmt(b, vals[i]);
        printf("f64[%d]=%.*s\n", i, (int)n, b);
    }

    /* --- 区域：合法跨区存（浅对象存入更深存储）不 trap --- */
    aic_region_push("<smoke>", 7);
    aic_guard(xs, aic_depth, "<>", 7); /* xs 任务区域(depth 0) ≤ 当前 depth */
    smoke_obj *tmp = (smoke_obj *)aic_alloc_hdr(sizeof(smoke_obj), 7);
    tmp->v = 42;
    printf("region depth=%u tmp=%d\n", (unsigned)aic_depth, tmp->v);
    aic_region_pop();
    printf("depth after pop=%u\n", (unsigned)aic_depth);

    printf("done\n");
    return 0;
}
