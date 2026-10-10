/* B9 分发核 · 接口虚调用（C 基线，-O2）。
 * 与 benches/b9_dispatch.aic 逐行同构：见证表两字 {data, vt}，同样的调用/累加次序。
 *
 * 【2026-10 修基线失真】两代都错在编译器把被测循环消灭掉：
 *   ① 第一版：drive 未加属性，gcc -O2 经 IPA 把 vt 常量传入、内联 counter_add
 *      并把循环闭合成 `leal` 算术（acc = init + 2*times，r+direct 也是闭式）
 *      ⇒ 循环一次都不执行，基线只剩 printf/启动时间。
 *   ② 第二版：加 noinline+noclone 后 gcc 仍**投机内联**：prologue 里
 *      `leaq counter_add` + 循环头 `cmpq %r12,%rbp; je 内联快路径`
 *      ⇒ 绝大多数迭代走内联分支，间接调用的真实成本没被测到。
 * 现在把 vt/data 放进 volatile 局部：编译器必须每轮从内存重新装载函数指针，
 * 既不能常量传播也不能投机 ⇒ 测到的就是"接口虚分发"本身的成本。
 * 语义不变（仍收一个 Adder、循环里经 vt 调用 times 次、同一组实参与次序）。 */
#include <stdio.h>
#include <stdlib.h>

typedef struct Counter { long n; } Counter;

static long counter_add(Counter *c, long d) {
    c->n += d;
    return c->n;
}

typedef long (*add_fn)(void *, long);
typedef struct { void *data; add_fn vt; } Adder;

static Adder adder_of(Counter *c) {
    Adder a;
    a.data = c;
    a.vt = (add_fn)counter_add;
    return a;
}

/* volatile 局部 = 每轮重新装载 vt/data，编译器不许把间接调用消掉（见头注）。 */
__attribute__((noinline, noclone))
static long drive(Adder a, long times) {
    volatile add_fn vt = a.vt;
    void *volatile data = a.data;
    long acc = 0;
    for (long i = 0; i < times; i++) acc = ((add_fn)vt)((void *)data, 2);
    return acc;
}

int main(int argc, char **argv) {
    long times = 2000000;
    if (argc >= 2) times = atol(argv[1]);
    Counter c = {1};
    Adder acc = adder_of(&c);
    long r = drive(acc, times);
    long direct = drive(adder_of(&c), times);
    printf("%ld\n", r + direct);
    return 0;
}
