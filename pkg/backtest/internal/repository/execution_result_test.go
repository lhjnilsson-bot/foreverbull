package repository_test

import (
	"context"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

func (test *ExecutionTest) TestStoreResultKeepsTheReturnBasisAndDividends() {
	ctx := context.Background()
	executions := repository.Execution{Conn: test.conn}
	created, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	benchmarkPrice := 0.05
	period := &backtest.Period{
		Date:                  &pb.Date{Year: 2024, Month: 7, Day: 31},
		AlgorithmPeriodReturn: 0.128124,
		PriceReturn:           0.111963,
		DividendsEarned:       97,
		BenchmarkPriceReturn:  &benchmarkPrice,
	}
	test.Require().NoError(executions.StoreResult(ctx, created.Id, backtest.ReturnBasis_RETURN_BASIS_TOTAL, 2, []*backtest.Period{period}))

	basis, foreignGross, err := executions.GetReturnBasis(ctx, created.Id)
	test.Require().NoError(err)
	test.Equal(backtest.ReturnBasis_RETURN_BASIS_TOTAL, basis)
	test.Equal(int32(2), foreignGross)

	periods, err := executions.GetPeriods(ctx, created.Id)
	test.Require().NoError(err)
	test.Require().Len(periods, 1)
	test.InDelta(0.128124, periods[0].AlgorithmPeriodReturn, 1e-12)
	test.InDelta(0.111963, periods[0].PriceReturn, 1e-12)
	test.InDelta(97, periods[0].DividendsEarned, 1e-12)
	test.Require().NotNil(periods[0].BenchmarkPriceReturn)
	test.InDelta(0.05, *periods[0].BenchmarkPriceReturn, 1e-12)
}

func (test *ExecutionTest) TestARunStoredBeforeDividendsHasNoReturnBasis() {
	ctx := context.Background()
	executions := repository.Execution{Conn: test.conn}
	created, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	test.Require().NoError(executions.StorePeriods(ctx, created.Id, []*backtest.Period{{Date: &pb.Date{Year: 2024, Month: 1, Day: 2}}}))
	_, err = test.conn.Exec(ctx, `UPDATE backtest_execution_period SET price_return=NULL, benchmark_price_return=NULL WHERE backtest_execution=$1`, created.Id)
	test.Require().NoError(err)

	basis, foreignGross, err := executions.GetReturnBasis(ctx, created.Id)
	test.Require().NoError(err)
	test.Equal(backtest.ReturnBasis_RETURN_BASIS_UNSPECIFIED, basis)
	test.Equal(int32(0), foreignGross)
	periods, err := executions.GetPeriods(ctx, created.Id)
	test.Require().NoError(err)
	test.Nil(periods[0].BenchmarkPriceReturn)
}

func (test *ExecutionTest) TestStoreResultStoresNothingWhenAPeriodFails() {
	ctx := context.Background()
	executions := repository.Execution{Conn: test.conn}
	created, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	period := &backtest.Period{Date: &pb.Date{Year: 2024, Month: 1, Day: 2}}

	err = executions.StoreResult(ctx, created.Id, backtest.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE, 0, []*backtest.Period{period, period})
	test.Require().ErrorContains(err, "unique_backtest_execution_period")

	basis, _, err := executions.GetReturnBasis(ctx, created.Id)
	test.Require().NoError(err)
	test.Equal(backtest.ReturnBasis_RETURN_BASIS_UNSPECIFIED, basis)
	periods, err := executions.GetPeriods(ctx, created.Id)
	test.Require().NoError(err)
	test.Empty(periods)
}

func (test *ExecutionTest) TestStoreResultRefusesAnUnspecifiedBasis() {
	ctx := context.Background()
	executions := repository.Execution{Conn: test.conn}
	created, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	err = executions.StoreResult(ctx, created.Id, backtest.ReturnBasis_RETURN_BASIS_UNSPECIFIED, 0, nil)
	test.Require().ErrorContains(err, "unknown return basis")
	err = executions.StoreResult(ctx, "missing", backtest.ReturnBasis_RETURN_BASIS_TOTAL, 0, nil)
	test.Require().ErrorContains(err, "no rows")
}
