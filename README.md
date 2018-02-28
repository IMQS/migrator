# Database migrator

This program consumes a directory containing one or more files with an extension of `.sql`.
The lexical order of the files determines the order of the migrations. Once a migration runs successfully, it's name is stored inside a table called `schema_migrations`.

If the database does not exist, then the migration will create the database.

## Build
    vgo build
(vgo must be installed. vgo needs Go 1.10)

## Run
./migrator logfile db sqlfiles

_logfile_ is the path of the log file  
_db_ is a connection string of the form driver:host:port:database:username:password  
_sqlfiles_ is a directory containing the SQL migration files  

## Naming Conventions of SQL Files
The SQL migration files must follow a strict naming convention. A typical set of migration files looks like this:
```
0000-0000.sql
0000-0001.sql
0000-0037.sql
2018-01-15-a-new-thing.sql
2018-01-18-team-a-bugfix.sql
2018-01-18-team-b-feature.sql
2018-02-03-more-things.sql
```

* Migrations are always run in lexical order
* Migrations must end in .sql (lower case)

There are two types of migrations. *Legacy* migrations always start with `0000-`, and are followed by a legacy migration number, which can be up to 4 digits. In the above example, there are three legacy migrations: `0000`, `0001`, and `0037`.

If this set of files was run on a new database, then all of the files listed above would be executed in the order that you see them.
However, the purpose of the legacy migrations is to bridge the gap from the old Albion migrator to this system. If this migrator sees a database that is currently under control of the Albion system, then it will not run the legacy migrations, because those migrations have already been run. For example, for the IMQS `main` database, there are about 160 migrations, and we do not need to re-run those.

The maximum legacy migration number that is found in the `.sql` files, is checked against the maximum legacy migration number in the database (in the `schema_migrations` table). Only if those two numbers match, does the upgrade proceed. In the above example, the maximum legacy migration number is 37. So only if we find entry `37` inside the schema_migrations table, will be proceed with the upgrade.

How do we detect if the system has been upgraded? The Albion system used a table called schema_migrations with a single field inside it called `version`. This new system uses the exact same names for the table and column, so how do we tell the two apart? The key difference is that the Albion system's `schema_migrations.version` field was an `integer`, and in our new system, it is `varchar`.

### Merge Conflicts
This system makes no attempt to prevent different teams from committing simultaneous migrations that conflict with each other. In our experience, this is not very likely to occur in practice, and the burden of having to serialize all migrations is not worth the protection that it brings. Even if there are ordering conflicts, they will only occur on CI machines. Remember that even though two teams may have merged their migrations in at different times, there is still a global ordering to the migrations, which is governed by their file names. So long as your releases are serialized, all production servers will replay the migrations in the same order.