#include <stdio.h>

static int add(int a, int b)
{
	int sum = a + b;
	return sum;
}

int main(int argc, char **argv)
{
	int i;
	int total = 0;

	for (i = 0; i < 3; i++)
		total = add(total, i);

	printf("total=%d argc=%d\n", total, argc);
	return 0;
}
