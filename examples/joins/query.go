// Package joins demonstrates a tenant-filtered employee/department projection.
package joins

import (
	"context"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/gorm"
)

type Employee struct {
	ID           uint
	Name         string `gorm:"size:100"`
	DepartmentID *uint
	TenantID     uint
	DeletedAt    gorm.DeletedAt
}

type Department struct {
	ID        uint
	Name      string `gorm:"size:100"`
	TenantID  uint
	DeletedAt gorm.DeletedAt
}

type EmployeeDepartment struct {
	EmployeeID     uint    `json:"employeeId"`
	EmployeeName   string  `json:"employeeName"`
	DepartmentName *string `json:"departmentName"`
}

var (
	employeeName = gormx.Field(func(e *Employee) *string { return &e.Name })
	employeeID   = gormx.Field(func(e *Employee) *uint { return &e.ID })
	departmentID = gormx.Field(func(e *Employee) **uint { return &e.DepartmentID })
)

// Page returns joined rows. TenantID must come from authenticated application context.
// A missing, deleted or filtered department leaves DepartmentName nil.
func Page(ctx context.Context, db *gorm.DB, tenantID uint, name string, request gormx.PageRequest) (*gormx.Page[EmployeeDepartment], error) {
	e := gormx.TableOf[Employee]("e")
	d := gormx.TableOf[Department]("d")
	query, err := gormx.NewJoinQuery(db, e)
	if err != nil {
		return nil, err
	}
	query = query.
		LeftJoin(d, gormx.On(gormx.Bind(e, departmentID), d.Col("ID")).
			And(d.Col("TenantID").Eq(tenantID))).
		Where(e.Col("TenantID").Eq(tenantID)).
		WhereIf(name != "", gormx.Bind(e, employeeName).Like("%"+name+"%")).
		Select(
			gormx.Bind(e, employeeID).As("employee_id"),
			gormx.Bind(e, employeeName).As("employee_name"),
			d.Col("Name").As("department_name"),
		).
		OrderBy(gormx.Bind(e, employeeID).Asc())
	return gormx.JoinPage[EmployeeDepartment](ctx, query, request)
}
