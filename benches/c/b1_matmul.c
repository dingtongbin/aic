/* B1 数值核 · f64 矩阵乘（C 基线，-O2）。
 * 与 benches/b1_matmul.aic **逐行同构**：扁平一维数组、同样的循环次序、
 * 同样的校验和输出（否则比的是算法不是后端，§九）。 */
#include <stdio.h>
#include <stdlib.h>

static void matmul(long n, double *a, double *b, double *c) {
    for (long i = 0; i < n; i++) {
        for (long j = 0; j < n; j++) {
            double sum = 0.0;
            for (long k = 0; k < n; k++) {
                sum += a[i * n + k] * b[k * n + j];
            }
            c[i * n + j] = sum;
        }
    }
}

int main(int argc, char **argv) {
    long n = 512;
    if (argc >= 2) n = atol(argv[1]);
    long size = n * n;
    double *a = malloc((size_t)size * sizeof(double));
    double *b = malloc((size_t)size * sizeof(double));
    double *c = malloc((size_t)size * sizeof(double));
    for (long i = 0; i < size; i++) { a[i] = 1.5; b[i] = 2.5; c[i] = 0.0; }
    matmul(n, a, b, c);
    double checksum = 0.0;
    for (long i = 0; i < size; i++) checksum += c[i];
    printf("%.6f\n", checksum);
    free(a); free(b); free(c);
    return 0;
}
