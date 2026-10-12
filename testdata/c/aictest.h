#ifndef AICTEST_H
#define AICTEST_H

/* 供 extern from 测试的最小 C 头：映射表各形态各至少一次 */

typedef struct aictest_point { int x; int y; } aictest_point;

int aictest_add(int a, int b);
int aictest_area(aictest_point p);
const char *aictest_name(void);

#define AICTEST_MAGIC 42

#endif /* AICTEST_H */
